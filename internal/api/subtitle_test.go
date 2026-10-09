package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/jobs"
	"github.com/ykzird/astraeus/internal/streaming"
	"github.com/ykzird/astraeus/internal/subtitles"
)

// fakeConverter writes a canned WebVTT file instead of running ffmpeg. It
// records which entry point the handler chose, which is how the tests tell a
// text extraction from an OCR pass, and it can claim or deny an OCR engine.
type fakeConverter struct {
	lastPath       string
	lastTrack      int
	lastImagePath  string
	lastImageTrack int
	imageErr       error
	ocrReady       bool
	err            error
}

func (f *fakeConverter) Convert(_ context.Context, mediaPath string, trackIndex int) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.lastPath = mediaPath
	f.lastTrack = trackIndex
	return writeCannedVTT()
}

func (f *fakeConverter) ConvertImage(_ context.Context, mediaPath string, trackIndex int) (string, error) {
	f.lastImagePath = mediaPath
	f.lastImageTrack = trackIndex
	if f.imageErr != nil {
		return "", f.imageErr
	}
	return writeCannedVTT()
}

func (f *fakeConverter) OCRReady() bool { return f.ocrReady }

// writeCannedVTT stands in for the conversion itself, so the tests can tell the
// two entry points apart by which fields were set rather than by the output.
func writeCannedVTT() (string, error) {
	dir, err := os.MkdirTemp("", "astraeus-fake-subs")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "track.vtt")
	if err := os.WriteFile(path, []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello\n"), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// subtitledInfo is a media file with one text and one image-based track.
func subtitledInfo() *streaming.MediaInfo {
	return &streaming.MediaInfo{
		Container:  "matroska",
		VideoCodec: "h264",
		AudioCodec: "aac",
		Subtitles: []streaming.SubtitleTrack{
			{Index: 2, Codec: "subrip", Language: "en", Title: "English", Default: true, Text: true},
			{Index: 3, Codec: "hdmv_pgs_subtitle", Language: "fr", Text: false},
		},
	}
}

func TestPlayback_AdvertisesDeliverableSubtitleTracks(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	response := decodeBody[playbackResponse](t, recorder)

	if len(response.Subtitles) != 2 {
		t.Fatalf("got %d subtitle tracks, want 2", len(response.Subtitles))
	}

	text := response.Subtitles[0]
	if text.Label != "English" {
		t.Errorf("label = %q, want the track title", text.Label)
	}
	if text.URL != "/api/objects/"+response.ObjectID+"/subtitles/2.vtt" {
		t.Errorf("url = %q, want the extraction endpoint", text.URL)
	}
	if !text.Default {
		t.Error("the default flag was not carried through")
	}

	image := response.Subtitles[1]
	if image.URL != "" {
		t.Errorf("image-based track url = %q, want it omitted", image.URL)
	}
	if image.Text {
		t.Error("a PGS track must not be reported as text")
	}
	if image.Label != "FR" {
		t.Errorf("label = %q, want the upper-cased language", image.Label)
	}

	// The advertised URL must actually serve WebVTT.
	recorder = env.do(t, http.MethodGet, text.URL, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("serving subtitles: status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/vtt") {
		t.Errorf("content type = %q, want text/vtt", got)
	}
	if !strings.HasPrefix(recorder.Body.String(), "WEBVTT") {
		t.Errorf("body is not WebVTT: %q", recorder.Body.String())
	}
	if converter.lastTrack != 2 {
		t.Errorf("converted track = %d, want 2", converter.lastTrack)
	}
}

func TestPlayback_AdvertisesImageTrackURLsWhenOCRIsAvailable(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{ocrReady: true}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	response := decodeBody[playbackResponse](t, recorder)

	image := response.Subtitles[1]
	// The source really is a picture, so `text` stays false; what changed is
	// that the server can now hand the client words for it.
	if image.Text {
		t.Error("a PGS track must stay text: false even when OCR can read it")
	}
	if image.URL != "/api/objects/"+response.ObjectID+"/subtitles/3.vtt" {
		t.Errorf("image track url = %q, want the OCR endpoint", image.URL)
	}

	// The advertised URL must actually serve WebVTT by the OCR path.
	recorder = env.do(t, http.MethodGet, image.URL, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("serving OCR subtitles: status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if converter.lastImageTrack != 3 {
		t.Errorf("OCR ran for track %d, want 3", converter.lastImageTrack)
	}
	if !strings.HasPrefix(recorder.Body.String(), "WEBVTT") {
		t.Errorf("body is not WebVTT: %q", recorder.Body.String())
	}
}

func TestPlayback_OmitsSubtitleURLsWithoutAConverter(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	response := decodeBody[playbackResponse](t, recorder)

	// The tracks are still reported; only the URL is withheld.
	if len(response.Subtitles) != 2 {
		t.Fatalf("got %d subtitle tracks, want 2", len(response.Subtitles))
	}
	for _, track := range response.Subtitles {
		if track.URL != "" {
			t.Errorf("track %d advertises %q without a converter", track.Index, track.URL)
		}
	}
}

func TestSubtitleEndpoint_RejectsImageBasedTracks(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/3.vtt", "")
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "subtitle_format_unsupported" {
		t.Errorf("error code = %q, want subtitle_format_unsupported", code)
	}
	if converter.lastTrack != 0 || converter.lastImageTrack != 0 {
		t.Error("conversion must not be attempted for an image-based track without an OCR engine")
	}
}

// TestSubtitleEndpoint_OCRsImageTracksWhenAvailable is the routing half of the
// OCR work: an image track goes through ConvertImage rather than the text
// extractor, and its words come back as a servable WebVTT track.
func TestSubtitleEndpoint_OCRsImageTracksWhenAvailable(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{ocrReady: true}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/3.vtt", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/vtt") {
		t.Errorf("content type = %q, want text/vtt", got)
	}
	if converter.lastImageTrack != 3 {
		t.Errorf("OCR ran for track %d, want 3", converter.lastImageTrack)
	}
	if converter.lastTrack != 0 {
		t.Error("the text extractor must not be used for an image track")
	}
}

// TestSubtitleEndpoint_OCRRefusalStillMapsTo415 covers the race where the OCR
// engine disappears between the capability check and the conversion: the
// endpoint keeps the explicit refusal instead of reporting a server fault.
func TestSubtitleEndpoint_OCRRefusalStillMapsTo415(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{ocrReady: true, imageErr: subtitles.ErrUnsupportedFormat}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/3.vtt", "")
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "subtitle_format_unsupported" {
		t.Errorf("error code = %q, want subtitle_format_unsupported", code)
	}
}

// vobsubInfo is a media file whose only subtitle track is a VobSub image track.
func vobsubInfo() *streaming.MediaInfo {
	return &streaming.MediaInfo{
		Container:  "matroska",
		VideoCodec: "h264",
		AudioCodec: "aac",
		Subtitles: []streaming.SubtitleTrack{
			{Index: 2, Codec: "dvd_subtitle", Language: "en", Text: false},
		},
	}
}

// dvbInfo is a media file whose only subtitle track is a DVB image track, a
// format the reader has no decoder for.
func dvbInfo() *streaming.MediaInfo {
	return &streaming.MediaInfo{
		Container:  "mpegts",
		VideoCodec: "h264",
		AudioCodec: "aac",
		Subtitles: []streaming.SubtitleTrack{
			{Index: 2, Codec: "dvb_subtitle", Language: "en", Text: false},
		},
	}
}

// TestSubtitleEndpoint_KeepsUndecodableImageTracksBurnOnly pins the boundary of
// the OCR work: the reader decodes PGS and VobSub, so a DVB track must keep the
// old 415 and stay without a URL even when an OCR engine is installed.
// Advertising it and then failing inside the extractor would be worse than not
// offering it.
func TestSubtitleEndpoint_KeepsUndecodableImageTracksBurnOnly(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{ocrReady: true}
	env := newTestEnv(t,
		withProber(stubProber{info: dvbInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	response := decodeBody[playbackResponse](t, recorder)
	if len(response.Subtitles) != 1 {
		t.Fatalf("got %d subtitle tracks, want 1", len(response.Subtitles))
	}
	if response.Subtitles[0].URL != "" {
		t.Errorf("DVB track url = %q, want it withheld as burn-only", response.Subtitles[0].URL)
	}

	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}
	recorder = env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/2.vtt", "")
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "subtitle_format_unsupported" {
		t.Errorf("error code = %q, want subtitle_format_unsupported", code)
	}
	if converter.lastImageTrack != 0 {
		t.Error("OCR must not be attempted for a codec the reader cannot decode")
	}
}

// TestSubtitleEndpoint_OCRsVobSubTracks covers the second image-subtitle reader:
// a VobSub track is offered as an ordinary text track and served through the
// same OCR entry point as PGS, rather than kept burn-only.
func TestSubtitleEndpoint_OCRsVobSubTracks(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{ocrReady: true}
	env := newTestEnv(t,
		withProber(stubProber{info: vobsubInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	response := decodeBody[playbackResponse](t, recorder)
	if len(response.Subtitles) != 1 {
		t.Fatalf("got %d subtitle tracks, want 1", len(response.Subtitles))
	}
	if response.Subtitles[0].Text {
		t.Error("a VobSub track must stay text: false even when OCR can read it")
	}
	want := "/api/objects/" + response.ObjectID + "/subtitles/2.vtt"
	if response.Subtitles[0].URL != want {
		t.Errorf("VobSub track url = %q, want %q", response.Subtitles[0].URL, want)
	}

	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}
	recorder = env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/2.vtt", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if converter.lastImageTrack != 2 {
		t.Errorf("OCR ran for track %d, want 2", converter.lastImageTrack)
	}
	if converter.lastTrack != 0 {
		t.Error("the text extractor must not be used for an image track")
	}
}

// TestSubtitleEndpoint_KeepsVobSubBurnOnlyWithoutAnEngine is the other half of
// the optional-dependency rule the PGS path already pins: a VobSub track the
// server could read is still a burn-in when no OCR engine is installed.
func TestSubtitleEndpoint_KeepsVobSubBurnOnlyWithoutAnEngine(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{}
	env := newTestEnv(t,
		withProber(stubProber{info: vobsubInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	response := decodeBody[playbackResponse](t, recorder)
	if len(response.Subtitles) != 1 {
		t.Fatalf("got %d subtitle tracks, want 1", len(response.Subtitles))
	}
	if response.Subtitles[0].URL != "" {
		t.Errorf("VobSub track url = %q without an OCR engine, want it withheld", response.Subtitles[0].URL)
	}

	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}
	recorder = env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/2.vtt", "")
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (body %s)", recorder.Code, recorder.Body.String())
	}
	if converter.lastImageTrack != 0 {
		t.Error("conversion must not be attempted without an OCR engine")
	}
}

func TestSubtitleEndpoint_UnknownTrack(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(&fakeConverter{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/7.vtt", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "track_not_found" {
		t.Errorf("error code = %q, want track_not_found", code)
	}
}

func TestSubtitleEndpoint_InvalidRequests(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(&fakeConverter{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}
	base := "/api/objects/" + objects[0].ID + "/subtitles/"

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "missing .vtt suffix", path: base + "2", wantStatus: http.StatusNotFound},
		{name: "non numeric track", path: base + "abc.vtt", wantStatus: http.StatusBadRequest},
		{name: "negative track", path: base + "-1.vtt", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := env.do(t, http.MethodGet, tt.path, "")
			if recorder.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestSubtitleEndpoint_UnavailableWithoutAConverter(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, withProber(stubProber{info: subtitledInfo()}), withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/2.vtt", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "subtitles_unavailable" {
		t.Errorf("error code = %q, want subtitles_unavailable", code)
	}
}

func TestSubtitleEndpoint_ExtractionErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		convertErr error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "track without cues",
			convertErr: subtitles.ErrNoCues,
			wantStatus: http.StatusNotFound,
			wantCode:   "no_subtitles",
		},
		{
			name:       "image-based format",
			convertErr: subtitles.ErrUnsupportedFormat,
			wantStatus: http.StatusUnsupportedMediaType,
			wantCode:   "subtitle_format_unsupported",
		},
		{
			name:       "ffmpeg failed",
			convertErr: errors.New("ffmpeg exploded"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "subtitle_extraction_failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newTestEnv(t,
				withProber(stubProber{info: subtitledInfo()}),
				withStreams(&fakeStreams{}),
				withSubtitles(&fakeConverter{err: tt.convertErr}))
			entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
			objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
			if err != nil {
				t.Fatalf("getting objects: %v", err)
			}

			recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/2.vtt", "")
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if code := decodeBody[errorBody](t, recorder).Code; code != tt.wantCode {
				t.Errorf("error code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

func TestSystemCapabilitiesReportsSubtitles(t *testing.T) {
	t.Parallel()

	withConverter := newTestEnv(t, withSubtitles(&fakeConverter{}))
	recorder := withConverter.do(t, http.MethodGet, "/api/system/capabilities", "")
	capabilities := decodeBody[systemCapabilitiesResponse](t, recorder)
	if !capabilities.SubtitlesEnabled {
		t.Error("subtitles_enabled = false, want true when a converter is configured")
	}
	if capabilities.SubtitleOCREnabled {
		t.Error("subtitle_ocr_enabled = true, want false without an OCR engine")
	}

	withOCR := newTestEnv(t, withSubtitles(&fakeConverter{ocrReady: true}))
	recorder = withOCR.do(t, http.MethodGet, "/api/system/capabilities", "")
	if !decodeBody[systemCapabilitiesResponse](t, recorder).SubtitleOCREnabled {
		t.Error("subtitle_ocr_enabled = false, want true with an OCR engine")
	}

	without := newTestEnv(t)
	recorder = without.do(t, http.MethodGet, "/api/system/capabilities", "")
	capabilities = decodeBody[systemCapabilitiesResponse](t, recorder)
	if capabilities.SubtitlesEnabled {
		t.Error("subtitles_enabled = true, want false without a converter")
	}
	if capabilities.SubtitleOCREnabled {
		t.Error("subtitle_ocr_enabled = true, want false without a converter")
	}
}

// slowConverter counts conversions and blocks until it is released, so a test
// can look at what happens while a recognition pass is in flight.
type slowConverter struct {
	fakeConverter
	mu      sync.Mutex
	runs    int
	release chan struct{}
	started chan struct{}
}

func (c *slowConverter) ConvertImage(ctx context.Context, mediaPath string, trackIndex int) (string, error) {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()

	if c.started != nil {
		select {
		case c.started <- struct{}{}:
		default:
		}
	}
	if c.release != nil {
		<-c.release
	}
	return writeCannedVTT()
}

func (c *slowConverter) runCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

// TestSubtitleEndpoint_OneOCRPassePerTrack is the regression test for the
// duplicate-work half of L-13.
//
// Every request for the same image track started its own recognition pass. The
// cache only helps after the first one has finished writing, so several viewers
// opening the same subtitle track ran several tesseract passes over the same
// bitmaps at once.
//
// The first request is held inside the conversion, and the other two are fired
// while it is held. That ordering is what makes the test measure deduplication
// rather than the scheduler: with the first pass blocked, a second pass can only
// start if the key was released, which is exactly the bug.
func TestSubtitleEndpoint_OneOCRPassePerTrack(t *testing.T) {
	t.Parallel()

	runner := jobs.New(context.Background(), jobs.Config{Workers: 2}, nil)
	t.Cleanup(runner.Close)

	converter := &slowConverter{
		fakeConverter: fakeConverter{ocrReady: true},
		started:       make(chan struct{}, 1),
		release:       make(chan struct{}),
	}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter),
		withJobs(runner))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}
	url := "/api/objects/" + objects[0].ID + "/subtitles/3.vtt"

	// Three viewers ask at once. None of them returns until the pass is released,
	// so the requests are all in flight while the assertions below run.
	var (
		mu    sync.Mutex
		codes []int
		wg    sync.WaitGroup
	)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code := env.do(t, http.MethodGet, url, "").Code
			mu.Lock()
			codes = append(codes, code)
			mu.Unlock()
		}()
	}

	select {
	case <-converter.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the recognition pass never started")
	}

	// The pass is held. If the other two requests started their own conversions
	// they would be inside ConvertImage now - the count is what says so, and the
	// window is long enough for them to get there.
	time.Sleep(200 * time.Millisecond)
	if got := converter.runCount(); got != 1 {
		t.Errorf("while one pass was held, %d recognition passes were running, want 1", got)
	}

	close(converter.release)
	wg.Wait()

	// And it stayed one pass for all three.
	if got := converter.runCount(); got != 1 {
		t.Errorf("three viewers on one track ran %d recognition passes, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(codes) != 3 {
		t.Fatalf("got %d responses, want 3", len(codes))
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("viewer %d got status %d, want 200: a joined pass still has to be served",
				i, code)
		}
	}
}

// TestSubtitleEndpoint_OCRPassOutlivesItsRequest is the regression test for the
// cancellation half of L-13.
//
// The conversion ran on r.Context(), so a client that gave up - the browser
// abandoning a slow response, or a viewer navigating away - cancelled an OCR pass
// that had already done most of its work. The pass runs on the runner's context,
// so it finishes and caches its result for whoever asks next.
func TestSubtitleEndpoint_OCRPassOutlivesItsRequest(t *testing.T) {
	t.Parallel()

	runner := jobs.New(context.Background(), jobs.Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	converter := &slowConverter{
		fakeConverter: fakeConverter{ocrReady: true},
		started:       make(chan struct{}, 1),
		release:       make(chan struct{}),
	}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter),
		withJobs(runner))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}

	// A request whose client goes away while the pass is running.
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet,
		"/api/objects/"+objects[0].ID+"/subtitles/3.vtt", nil).WithContext(ctx)

	served := make(chan struct{})
	go func() {
		defer close(served)
		env.server.Handler().ServeHTTP(httptest.NewRecorder(), request)
	}()

	select {
	case <-converter.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the recognition pass never started")
	}

	// The client gives up.
	cancel()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler did not return after its client went away")
	}

	// The pass is still running, and finishing it is what caches the result.
	close(converter.release)
	waitFor(t, "the recognition pass to finish", func() bool {
		return converter.runCount() == 1
	})

	// A second request is served from the result the abandoned pass produced,
	// which is the point: the work was not thrown away.
	if got := env.do(t, http.MethodGet,
		"/api/objects/"+objects[0].ID+"/subtitles/3.vtt", "").Code; got != http.StatusOK {
		t.Errorf("the follow-up request = %d, want 200", got)
	}
}

// TestSubtitleResponseIsPrivatelyCached is the other half of A-10's static-file
// item.
//
// The converted track was served `Cache-Control: public, max-age=86400`, and a
// subtitle track is reachable only through a library the viewer may see - so the
// answer belongs to one viewer's rights. `public` invites a shared cache, or a
// reverse proxy in front of the server, to keep it and hand it to somebody who has
// no access to that library at all.
func TestSubtitleResponseIsPrivatelyCached(t *testing.T) {
	t.Parallel()

	converter := &fakeConverter{ocrReady: true}
	env := newTestEnv(t,
		withProber(stubProber{info: subtitledInfo()}),
		withStreams(&fakeStreams{}),
		withSubtitles(converter))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	objects, err := env.repo.GetObjectsByEntity(context.Background(), entity.ID)
	if err != nil {
		t.Fatalf("getting objects: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/objects/"+objects[0].ID+"/subtitles/2.vtt", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	cacheControl := recorder.Header().Get("Cache-Control")
	if strings.Contains(cacheControl, "public") {
		t.Errorf("Cache-Control = %q, want it private: access to a subtitle track is "+
			"per library, so a shared cache must not keep the response", cacheControl)
	}
	if !strings.Contains(cacheControl, "private") {
		t.Errorf("Cache-Control = %q, want it to say private", cacheControl)
	}
}
