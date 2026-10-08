package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jok/astraeus-media/internal/library"
	"github.com/jok/astraeus-media/internal/streaming"
)

// stubProber returns a fixed MediaInfo so negotiation can be exercised without
// ffprobe.
type stubProber struct {
	info *streaming.MediaInfo
	err  error
}

func (s stubProber) Probe(context.Context, string) (*streaming.MediaInfo, error) {
	return s.info, s.err
}

// fakeStreams records Start calls without running ffmpeg.
type fakeStreams struct {
	started   int
	lastPath  string
	lastMode  streaming.PlaybackMode
	lastStart float64
	stopped   []string
	startErr  error
	servedOut string
}

func (f *fakeStreams) Start(ctx context.Context, entityID, objectPath string, decision streaming.Decision) (*streaming.Session, error) {
	return f.StartAt(ctx, entityID, objectPath, decision, 0)
}

func (f *fakeStreams) StartAt(_ context.Context, entityID, objectPath string, decision streaming.Decision, startSeconds float64) (*streaming.Session, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.started++
	f.lastPath = objectPath
	f.lastMode = decision.Mode
	f.lastStart = startSeconds
	return &streaming.Session{
		ID:           "fake-session",
		EntityID:     entityID,
		ObjectPath:   objectPath,
		Decision:     decision,
		StartSeconds: startSeconds,
	}, nil
}

func (f *fakeStreams) Session(id string) (*streaming.Session, bool) {
	if id != "fake-session" {
		return nil, false
	}
	return &streaming.Session{ID: id}, true
}

func (f *fakeStreams) Stop(id string) {
	f.stopped = append(f.stopped, id)
}

func (f *fakeStreams) ServeFile(w http.ResponseWriter, _ *http.Request, sessionID, name string) {
	if sessionID != "fake-session" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f.servedOut = name
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("segment-bytes"))
}

// seedPlayableEntity creates a library, a movie entity and a media object on
// disk, returning the entity and the real file path.
func seedPlayableEntity(t *testing.T, env *testEnv, fileName, content string) (library.MediaEntity, string) {
	t.Helper()

	root := t.TempDir()
	filePath := filepath.Join(root, fileName)
	writeMediaFile(t, filePath, content)

	lib := createLibrary(t, env, "Movies", root, "movies")
	env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")

	recorder := env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities", "")
	entities := decodeBody[[]library.MediaEntity](t, recorder)
	if len(entities) == 0 {
		t.Fatal("the scan produced no entities")
	}
	return entities[0], filePath
}

func TestPlayback_DirectPlay(t *testing.T) {
	t.Parallel()

	prober := stubProber{info: &streaming.MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080,
	}}
	streams := &fakeStreams{}
	env := newTestEnv(t, withProber(prober), withStreams(streams))

	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "not really a video")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	response := decodeBody[playbackResponse](t, recorder)
	if response.Mode != streaming.ModeDirectPlay {
		t.Errorf("mode = %q, want %q", response.Mode, streaming.ModeDirectPlay)
	}
	if response.URL != "/api/objects/"+response.ObjectID+"/file" {
		t.Errorf("url = %q, want the object file URL", response.URL)
	}
	if streams.started != 0 {
		t.Errorf("a streaming session was started for a direct-play decision")
	}
	if len(response.Decision.Reasons) == 0 {
		t.Error("the decision should explain itself")
	}

	// The direct-play URL must actually serve the file.
	recorder = env.do(t, http.MethodGet, response.URL, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("serving the media file: status = %d", recorder.Code)
	}
	if recorder.Body.String() != "not really a video" {
		t.Errorf("body = %q, want the file contents", recorder.Body.String())
	}
}

func TestPlayback_RemuxStartsASession(t *testing.T) {
	t.Parallel()

	// Matroska with browser-compatible streams: repackage, do not re-encode.
	prober := stubProber{info: &streaming.MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080,
	}}
	streams := &fakeStreams{}
	env := newTestEnv(t, withProber(prober), withStreams(streams))

	entity, filePath := seedPlayableEntity(t, env, "Dune (2021).mkv", "matroska bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	response := decodeBody[playbackResponse](t, recorder)
	if response.Mode != streaming.ModeRemux {
		t.Fatalf("mode = %q, want %q", response.Mode, streaming.ModeRemux)
	}
	if response.SessionID != "fake-session" {
		t.Errorf("session id = %q, want fake-session", response.SessionID)
	}
	if response.URL != "/hls/fake-session/playlist.m3u8" {
		t.Errorf("url = %q, want the session playlist", response.URL)
	}
	if streams.started != 1 {
		t.Fatalf("sessions started = %d, want 1", streams.started)
	}
	if streams.lastPath != filePath {
		t.Errorf("session input = %q, want %q", streams.lastPath, filePath)
	}

	// The advertised playlist URL must be served by the stream manager.
	recorder = env.do(t, http.MethodGet, response.URL, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("serving the playlist: status = %d", recorder.Code)
	}
	if streams.servedOut != "playlist.m3u8" {
		t.Errorf("served %q, want playlist.m3u8", streams.servedOut)
	}
}

func TestPlayback_DeclaredCapabilityDrivesTheDecision(t *testing.T) {
	t.Parallel()

	prober := stubProber{info: &streaming.MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 3840, Height: 2160,
	}}
	streams := &fakeStreams{}
	env := newTestEnv(t, withProber(prober), withStreams(streams))

	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "bytes")

	// A client that only accepts 720p forces a downscaling transcode.
	body := `{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"max_height":720,"supports_hls":true}`
	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	response := decodeBody[playbackResponse](t, recorder)
	if response.Mode != streaming.ModeTranscode {
		t.Fatalf("mode = %q, want %q", response.Mode, streaming.ModeTranscode)
	}
	if response.Decision.TargetHeight != 720 {
		t.Errorf("target height = %d, want 720", response.Decision.TargetHeight)
	}
	if streams.lastMode != streaming.ModeTranscode {
		t.Errorf("the stream manager was asked for %q", streams.lastMode)
	}
}

// TestPlayback_PreferredHeightAsksForALadder pins the request field end to end,
// including its JSON name: a quality choice expressed as a preference must come
// back as an adaptive ladder topped at that height, while max_height keeps
// pinning a single rendition.
func TestPlayback_PreferredHeightAsksForALadder(t *testing.T) {
	t.Parallel()

	prober := stubProber{info: &streaming.MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 3840, Height: 2160,
	}}
	streams := &fakeStreams{}
	env := newTestEnv(t, withProber(prober), withStreams(streams))

	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "bytes")

	body := `{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"preferred_height":720,"supports_hls":true}`
	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	response := decodeBody[playbackResponse](t, recorder)
	if response.Mode != streaming.ModeTranscode {
		t.Fatalf("mode = %q, want %q", response.Mode, streaming.ModeTranscode)
	}
	if len(response.Decision.Renditions) < 2 {
		t.Fatalf("renditions = %+v, want a ladder for a preferred height", response.Decision.Renditions)
	}
	if response.Decision.Renditions[0].Height != 720 {
		t.Errorf("the top rung is %d, want the preferred 720", response.Decision.Renditions[0].Height)
	}
	if response.Decision.TargetHeight != 720 {
		t.Errorf("target height = %d, want the top rung 720", response.Decision.TargetHeight)
	}

	// And max_height on its own still means one rendition: the historical
	// contract a client asking for determinism relies on.
	pinnedBody := `{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"max_height":720,"supports_hls":true}`
	recorder = env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", pinnedBody)
	pinned := decodeBody[playbackResponse](t, recorder)
	if len(pinned.Decision.Renditions) != 0 {
		t.Errorf("max_height alone produced %+v, want one rendition", pinned.Decision.Renditions)
	}
}

func TestPlayback_UndeliverableIsAConflict(t *testing.T) {
	t.Parallel()

	prober := stubProber{info: &streaming.MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
	}}
	env := newTestEnv(t, withProber(prober), withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	// The client cannot play HLS, so the repackaged stream has nowhere to go.
	body := `{"containers":["mp4"],"video_codecs":["h264"],"audio_codecs":["aac"],"supports_hls":false}`
	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", body)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "not_deliverable" {
		t.Errorf("error code = %q, want not_deliverable", code)
	}
}

func TestPlayback_InvalidCapability(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, withProber(stubProber{info: &streaming.MediaInfo{Container: "mp4", VideoCodec: "h264"}}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", `{"containers":["mp4"]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestPlayback_UnavailableDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  func(t *testing.T) *testEnv
	}{
		{
			name: "no prober",
			env:  func(t *testing.T) *testEnv { return newTestEnv(t) },
		},
		{
			name: "no stream manager",
			env: func(t *testing.T) *testEnv {
				return newTestEnv(t, withProber(stubProber{info: &streaming.MediaInfo{
					Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
				}}))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := tt.env(t)
			entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

			recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
			}
			if code := decodeBody[errorBody](t, recorder).Code; code != "streaming_unavailable" {
				t.Errorf("error code = %q, want streaming_unavailable", code)
			}
		})
	}
}

func TestPlayback_ProbeFailureIsAServerError(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t,
		withProber(stubProber{err: errors.New("ffprobe exploded")}),
		withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestPlayback_ContainerEntityHasNoMedia(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t,
		withProber(stubProber{info: &streaming.MediaInfo{Container: "mp4", VideoCodec: "h264"}}),
		withStreams(&fakeStreams{}))

	root := t.TempDir()
	writeMediaFile(t, filepath.Join(root, "Show", "Season 01", "Show.S01E01.mkv"), "bytes")
	lib := createLibrary(t, env, "Shows", root, "shows")
	env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")

	recorder := env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities", "")
	entities := decodeBody[[]library.MediaEntity](t, recorder)
	var series library.MediaEntity
	for _, entity := range entities {
		if entity.Type == library.SeriesEntity {
			series = entity
		}
	}
	if series.ID == "" {
		t.Fatal("no series entity was created")
	}

	recorder = env.do(t, http.MethodPost, "/api/entities/"+series.ID+"/playback", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "no_media" {
		t.Errorf("error code = %q, want no_media", code)
	}
}

func TestPlayback_StartFailureIsReported(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t,
		withProber(stubProber{info: &streaming.MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "aac"}}),
		withStreams(&fakeStreams{startErr: errors.New("ffmpeg is missing")}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "stream_start_failed" {
		t.Errorf("error code = %q, want stream_start_failed", code)
	}
}

func TestObjectFile_MissingOnDisk(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	entity, filePath := seedPlayableEntity(t, env, "Dune (2021).mp4", "bytes")

	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(objects))
	}

	// The row still exists but the file behind it is gone.
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("removing media file: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/file", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "file_missing" {
		t.Errorf("error code = %q, want file_missing", code)
	}
}

func TestObjectFile_NotFound(t *testing.T) {
	t.Parallel()

	recorder := newTestEnv(t).do(t, http.MethodGet, "/api/objects/nope/file", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestStreamFile_WithoutManager(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	recorder := env.do(t, http.MethodGet, "/hls/whatever/playlist.m3u8", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestSystemCapabilities(t *testing.T) {
	t.Parallel()

	capability := streaming.ServerCapability{
		FFmpegAvailable:      true,
		FFprobeAvailable:     true,
		HardwareAcceleration: []string{"qsv"},
		VideoEncoders:        []string{"h264_qsv", "libx264"},
		HLS:                  true,
	}
	env := newTestEnv(t, withServerCapability(capability), withProber(stubProber{}), withStreams(&fakeStreams{}))

	recorder := env.do(t, http.MethodGet, "/api/system/capabilities", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	response := decodeBody[systemCapabilitiesResponse](t, recorder)
	if len(response.HardwareAcceleration) != 1 || response.HardwareAcceleration[0] != "qsv" {
		t.Errorf("hardware acceleration = %v, want [qsv]", response.HardwareAcceleration)
	}
	if !response.NegotiationEnabled || !response.SegmentingEnabled {
		t.Errorf("expected both negotiation and segmenting to be reported as enabled: %+v", response)
	}
}

// TestPlayback_StartSeconds covers resuming at an offset, which is what a
// quality change and a seek past produced content both rely on.
func TestPlayback_StartSeconds(t *testing.T) {
	t.Parallel()

	transcodeInfo := &streaming.MediaInfo{
		Container: "mp4", VideoCodec: "hevc", AudioCodec: "ac3",
		Width: 3840, Height: 1600, DurationSeconds: 6000, AudioChannels: 6,
	}

	t.Run("an offset reaches the stream and is echoed back", func(t *testing.T) {
		t.Parallel()

		streams := &fakeStreams{}
		env := newTestEnv(t, withProber(stubProber{info: transcodeInfo}), withStreams(streams))
		entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "not really a video")

		recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
			`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"max_height":720,"supports_hls":true,"start_seconds":600.5}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}

		if streams.lastStart != 600.5 {
			t.Errorf("stream started at %v, want 600.5", streams.lastStart)
		}
		response := decodeBody[playbackResponse](t, recorder)
		if response.StartSeconds != 600.5 {
			t.Errorf("response start_seconds = %v, want 600.5", response.StartSeconds)
		}
	})

	t.Run("a negative offset is refused", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t, withProber(stubProber{info: transcodeInfo}), withStreams(&fakeStreams{}))
		entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "not really a video")

		recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
			`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"max_height":720,"supports_hls":true,"start_seconds":-5}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "non-negative") {
			t.Errorf("the error should explain itself: %s", recorder.Body.String())
		}
	})

	t.Run("an offset past the end is refused before ffmpeg is asked", func(t *testing.T) {
		t.Parallel()

		streams := &fakeStreams{}
		env := newTestEnv(t, withProber(stubProber{info: &streaming.MediaInfo{
			Container: "mp4", VideoCodec: "hevc", Width: 3840, Height: 1600, DurationSeconds: 100,
		}}), withStreams(streams))
		entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "not really a video")

		recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
			`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"max_height":720,"supports_hls":true,"start_seconds":5000}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "past the end") {
			t.Errorf("the error should say why: %s", recorder.Body.String())
		}
		if streams.started != 0 {
			t.Error("a session was started for a start offset past the end of the media")
		}
	})
}

// TestStopStream covers the route a client uses to end a transcode at once.
// Without it, changing quality or closing the player leaves ffmpeg encoding
// frames nobody will watch until the idle reaper happens to notice.
func TestStopStream(t *testing.T) {
	t.Parallel()

	t.Run("an existing session is stopped", func(t *testing.T) {
		t.Parallel()

		streams := &fakeStreams{}
		env := newTestEnv(t, withStreams(streams))

		recorder := env.do(t, http.MethodDelete, "/api/streams/fake-session", "")
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %s)", recorder.Code, recorder.Body.String())
		}
		if len(streams.stopped) != 1 || streams.stopped[0] != "fake-session" {
			t.Errorf("stopped = %v, want [fake-session]", streams.stopped)
		}
	})

	t.Run("an unknown session is a 404", func(t *testing.T) {
		t.Parallel()

		streams := &fakeStreams{}
		env := newTestEnv(t, withStreams(streams))

		recorder := env.do(t, http.MethodDelete, "/api/streams/not-a-session", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
		}
		if len(streams.stopped) != 0 {
			t.Errorf("stopped = %v, want nothing stopped", streams.stopped)
		}
	})
}

// multiTrackProber is a film with two audio tracks, the second marked default.
func multiTrackProber() stubProber {
	return stubProber{info: &streaming.MediaInfo{
		Container: "matroska", VideoCodec: "h264",
		AudioCodec: "ac3", AudioChannels: 6, AudioBitrateKbps: 448,
		Width: 1920, Height: 1080, BitrateKbps: 8_000,
		AudioTracks: []streaming.AudioTrack{
			{Index: 1, Codec: "aac", Channels: 2, BitrateKbps: 128, Language: "eng"},
			{Index: 3, Codec: "ac3", Channels: 6, BitrateKbps: 448, Language: "deu", Default: true},
		},
	}}
}

// TestPlayback_UnknownAudioTrackIsRejected covers the API's half of the audio
// choice: a client that names a track the file does not have is told what it
// does have, rather than being quietly given a different track.
func TestPlayback_UnknownAudioTrackIsRejected(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, withProber(multiTrackProber()), withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "matroska bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"supports_hls":true,"audio_track_index":9}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"unknown_audio_track", "3 (deu ac3 6ch)", "1 (eng aac 2ch)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the rejection should tell the client what exists, missing %q: %s", want, body)
		}
	}
}

// TestPlayback_ChosenAudioTrackIsDelivered covers the accepted path: the track
// the client asks for is the one the decision names and the one a session would
// map.
func TestPlayback_ChosenAudioTrackIsDelivered(t *testing.T) {
	t.Parallel()

	streams := &fakeStreams{}
	env := newTestEnv(t, withProber(multiTrackProber()), withStreams(streams))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "matroska bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"supports_hls":true,"audio_track_index":1}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	response := decodeBody[playbackResponse](t, recorder)
	if response.Decision.TargetAudioStreamIndex != 1 {
		t.Errorf("target audio stream = %d, want 1", response.Decision.TargetAudioStreamIndex)
	}
	if response.Decision.AudioAction != streaming.ActionCopy {
		t.Errorf("audio action = %q, want copy: track 1 is stereo AAC", response.Decision.AudioAction)
	}
	// The response carries the whole track list, so a player can offer the
	// choice without probing the file itself.
	if len(response.MediaInfo.AudioTracks) != 2 {
		t.Errorf("media_info.audio_tracks = %+v, want both tracks", response.MediaInfo.AudioTracks)
	}
	if streams.started != 1 {
		t.Errorf("sessions started = %d, want 1", streams.started)
	}
}

// TestPlayback_NoAudioChoiceUsesTheDefaultTrack checks that saying nothing still
// means "the track the file marks default", not "the first one".
func TestPlayback_NoAudioChoiceUsesTheDefaultTrack(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, withProber(multiTrackProber()), withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "matroska bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"supports_hls":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	response := decodeBody[playbackResponse](t, recorder)
	if response.Decision.TargetAudioStreamIndex != 3 {
		t.Errorf("target audio stream = %d, want the default track's 3", response.Decision.TargetAudioStreamIndex)
	}
}

// TestPlayback_BurnsAnImageSubtitle covers the request field end to end: an
// image track is composited into the picture by a re-encode, a text track is
// refused with a message that points at track selection, and an index the file
// does not have is refused with the list of what it does have.
func TestPlayback_BurnsAnImageSubtitle(t *testing.T) {
	t.Parallel()

	info := &streaming.MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, DurationSeconds: 600,
		AudioTracks: []streaming.AudioTrack{{Index: 1, Codec: "aac", Channels: 2}},
		Subtitles: []streaming.SubtitleTrack{
			{Index: 2, Codec: "subrip", Text: true, Language: "en"},
			{Index: 3, Codec: "hdmv_pgs_subtitle", Text: false, Language: "fr"},
		},
	}
	env := newTestEnv(t, withProber(stubProber{info: info}), withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Blade Runner 2049 (2017).mkv", "matroska bytes")

	body := `{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],` +
		`"supports_hls":true,"subtitles":true,"burn_subtitle_index":3}`

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	response := decodeBody[playbackResponse](t, recorder)
	if response.Decision.BurnedSubtitleIndex != 3 {
		t.Errorf("burned index = %d, want 3", response.Decision.BurnedSubtitleIndex)
	}
	if response.Decision.VideoAction != streaming.ActionTranscode {
		t.Errorf("video action = %q, want a transcode", response.Decision.VideoAction)
	}

	// A text track named for burning is a category error, not a burn.
	recorder = env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],`+
			`"supports_hls":true,"burn_subtitle_index":2}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("text-track burn status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); !strings.Contains(got, "subtitle_not_image") {
		t.Errorf("expected subtitle_not_image, got %s", got)
	}

	// An index the file does not have names the tracks it does.
	recorder = env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],`+
			`"supports_hls":true,"burn_subtitle_index":9}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown-track burn status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	got := recorder.Body.String()
	if !strings.Contains(got, "unknown_subtitle_track") || !strings.Contains(got, "3 (fr hdmv_pgs_subtitle, image)") {
		t.Errorf("the refusal should list the tracks the file has, got %s", got)
	}

	// A negative index is a malformed capability.
	recorder = env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],`+
			`"supports_hls":true,"burn_subtitle_index":-1}`)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("negative index status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
}
