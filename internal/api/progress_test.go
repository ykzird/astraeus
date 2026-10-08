package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jok/astraeus-media/internal/streaming"
)

// TestPlaybackProgress_RoundTrip covers the report-and-resume loop: a client
// reports where it got to, and reading the entity afterwards says so.
func TestPlaybackProgress_RoundTrip(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	entity, _ := seedPlayableEntity(t, env, "Arrival (2016).mp4", "mp4 bytes")

	// Nothing reported yet: the detail carries no progress at all rather than a
	// zero position, so a client can tell "never played" from "played to 0:00".
	detail := decodeBody[entityDetail](t, env.do(t, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if detail.Progress != nil {
		t.Fatalf("unplayed entity has progress %+v, want none", detail.Progress)
	}

	recorder := env.do(t, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 754.5, "duration_seconds": 7025}`)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", recorder.Code, recorder.Body.String())
	}

	detail = decodeBody[entityDetail](t, env.do(t, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if detail.Progress == nil {
		t.Fatal("the reported position was not stored")
	}
	if detail.Progress.PositionSeconds != 754.5 || detail.Progress.DurationSeconds != 7025 {
		t.Errorf("progress = %+v, want 754.5 of 7025", detail.Progress)
	}
	if detail.Progress.Percent < 10 || detail.Progress.Percent > 11 {
		t.Errorf("percent = %v, want about 10.7", detail.Progress.Percent)
	}
	if detail.Progress.Finished {
		t.Error("a tenth of the way in is not finished")
	}

	// Reporting again replaces the position.
	env.do(t, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 3000, "duration_seconds": 7025}`)
	detail = decodeBody[entityDetail](t, env.do(t, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if detail.Progress.PositionSeconds != 3000 {
		t.Errorf("position = %v, want the latest report 3000", detail.Progress.PositionSeconds)
	}

	// Starting over forgets it.
	recorder = env.do(t, http.MethodDelete, "/api/entities/"+entity.ID+"/progress", "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", recorder.Code)
	}
	detail = decodeBody[entityDetail](t, env.do(t, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if detail.Progress != nil {
		t.Errorf("progress survived being cleared: %+v", detail.Progress)
	}
}

// TestPlaybackProgress_FinishedIsCleared covers the end of a film: a position in
// the closing minutes is dropped rather than stored, because resuming three
// seconds from the end is worse than starting the next thing.
func TestPlaybackProgress_FinishedIsCleared(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	entity, _ := seedPlayableEntity(t, env, "Arrival (2016).mp4", "mp4 bytes")

	// Start with a position that would be worth resuming.
	env.do(t, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 600, "duration_seconds": 600}`)

	// Then finish it: past FinishedFraction, the stored position goes.
	recorder := env.do(t, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 590, "duration_seconds": 600}`)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", recorder.Code, recorder.Body.String())
	}

	detail := decodeBody[entityDetail](t, env.do(t, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if detail.Progress != nil {
		t.Errorf("a finished film kept progress %+v, want none", detail.Progress)
	}
}

// TestPlaybackProgress_RejectsNonsense covers the numbers a player must not be
// able to store: a negative position, and one past the end of the media.
func TestPlaybackProgress_RejectsNonsense(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	entity, _ := seedPlayableEntity(t, env, "Arrival (2016).mp4", "mp4 bytes")

	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "negative position",
			body: `{"position_seconds": -1, "duration_seconds": 600}`,
			code: "invalid_position",
		},
		{
			name: "negative duration",
			body: `{"position_seconds": 10, "duration_seconds": -600}`,
			code: "invalid_duration",
		},
		{
			name: "position past the end",
			body: `{"position_seconds": 900, "duration_seconds": 600}`,
			code: "position_beyond_end",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := env.do(t, http.MethodPut, "/api/entities/"+entity.ID+"/progress", tt.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); !strings.Contains(body, tt.code) {
				t.Errorf("error code %q missing from %s", tt.code, body)
			}
		})
	}
}

// TestPlaybackProgress_UnknownEntityIs404 checks the handler names the entity it
// could not find rather than storing progress against a dangling id.
func TestPlaybackProgress_UnknownEntityIs404(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	recorder := env.do(t, http.MethodPut, "/api/entities/00000000-0000-0000-0000-000000000000/progress",
		`{"position_seconds": 10, "duration_seconds": 600}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// TestPlayback_DirectPlayDoesNotClaimToHonourAnOffset covers a report that would
// otherwise be a lie: direct play serves the original file whole, so a requested
// start is applied by the client seeking, not by the server. Echoing the offset
// would make a client build a timeline wrong by exactly that much.
func TestPlayback_DirectPlayDoesNotClaimToHonourAnOffset(t *testing.T) {
	t.Parallel()

	prober := stubProber{info: &streaming.MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, DurationSeconds: 600,
	}}
	env := newTestEnv(t, withProber(prober), withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Blade Runner 2049 (2017).mp4", "mp4 bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["mp4"],"video_codecs":["h264"],"audio_codecs":["aac"],"supports_hls":true,"start_seconds":300}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	response := decodeBody[playbackResponse](t, recorder)
	if response.Mode != streaming.ModeDirectPlay {
		t.Fatalf("mode = %q, want direct play", response.Mode)
	}
	if response.StartSeconds != 0 {
		t.Errorf("start_seconds = %v, want 0: this delivery begins at the start of the file",
			response.StartSeconds)
	}
	if !strings.Contains(strings.Join(response.Decision.Reasons, "; "), "applied by the client") {
		t.Errorf("the reasons should say who applies the offset: %s",
			strings.Join(response.Decision.Reasons, "; "))
	}

	// A segmented session really does begin at the offset, so the echo stays.
	segmented := stubProber{info: &streaming.MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, DurationSeconds: 600,
	}}
	env = newTestEnv(t, withProber(segmented), withStreams(&fakeStreams{}))
	entity, _ = seedPlayableEntity(t, env, "Blade Runner 2049 (2017).mkv", "matroska bytes")

	recorder = env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback",
		`{"containers":["hls"],"video_codecs":["h264"],"audio_codecs":["aac"],"supports_hls":true,"start_seconds":300}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	response = decodeBody[playbackResponse](t, recorder)
	if response.Mode == streaming.ModeDirectPlay {
		t.Fatalf("mode = %q, want a segmented mode", response.Mode)
	}
	if response.StartSeconds != 300 {
		t.Errorf("start_seconds = %v, want the requested 300 for a segmented session", response.StartSeconds)
	}
}

// TestListProgress_ListsWhatIsWorthResuming covers the continue-watching list:
// one request answers "what was I in the middle of", with the entity ready to
// render rather than an id to look up.
func TestListProgress_ListsWhatIsWorthResuming(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	entity, _ := seedPlayableEntity(t, env, "Arrival (2016).mp4", "mp4 bytes")

	recorder := env.do(t, http.MethodGet, "/api/progress", "")
	var empty struct {
		Entries []progressEntryResource `json:"entries"`
	}
	empty = decodeBody[struct {
		Entries []progressEntryResource `json:"entries"`
	}](t, recorder)
	if len(empty.Entries) != 0 {
		t.Fatalf("nothing played should list nothing, got %+v", empty.Entries)
	}

	env.do(t, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 1200, "duration_seconds": 7000}`)

	recorder = env.do(t, http.MethodGet, "/api/progress", "")
	body := decodeBody[struct {
		Entries []progressEntryResource `json:"entries"`
	}](t, recorder)
	if len(body.Entries) != 1 {
		t.Fatalf("entries = %+v, want the one position", body.Entries)
	}
	entry := body.Entries[0]
	if entry.Entity.ID != entity.ID || entry.Entity.Name != entity.Name {
		t.Errorf("entry entity = %+v, want %q", entry.Entity, entity.Name)
	}
	if entry.Progress == nil || entry.Progress.PositionSeconds != 1200 {
		t.Errorf("entry progress = %+v, want 1200", entry.Progress)
	}

	// A limit the client chooses is honoured, and one it cannot mean is refused.
	if recorder := env.do(t, http.MethodGet, "/api/progress?limit=1", ""); recorder.Code != http.StatusOK {
		t.Errorf("limit=1 status = %d, want 200", recorder.Code)
	}
	recorder = env.do(t, http.MethodGet, "/api/progress?limit=0", "")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("limit=0 status = %d, want 400", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "invalid_limit") {
		t.Errorf("expected an invalid_limit code, got %s", body)
	}
}
