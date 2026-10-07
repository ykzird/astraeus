package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jok/astraeus-media/internal/streaming"
)

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	// Generate a little traffic first.
	env.do(t, http.MethodGet, "/api/health", "")
	env.do(t, http.MethodGet, "/api/health", "")
	env.do(t, http.MethodGet, "/api/entities/nope", "")

	recorder := env.do(t, http.MethodGet, "/metrics", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content type = %q, want the Prometheus text format", got)
	}

	body := recorder.Body.String()
	for _, want := range []string{
		"# TYPE astraeus_http_requests_total counter",
		`astraeus_http_requests_total{method="GET",status="200"} 2`,
		`astraeus_http_requests_total{method="GET",status="404"} 1`,
		"# TYPE astraeus_http_request_seconds histogram",
		"astraeus_http_request_seconds_count",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output is missing %q\n---\n%s", want, body)
		}
	}
}

func TestMetrics_RecordsPlaybackDecisions(t *testing.T) {
	t.Parallel()

	prober := stubProber{info: &streaming.MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080,
	}}
	env := newTestEnv(t, withProber(prober), withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("playback status = %d, want 200", recorder.Code)
	}

	recorder = env.do(t, http.MethodGet, "/metrics", "")
	body := recorder.Body.String()

	if !strings.Contains(body, `astraeus_playback_decisions_total{mode="direct_play"} 1`) {
		t.Errorf("the playback decision was not counted:\n%s", body)
	}
}

func TestMetrics_RecordsProbeFailures(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t,
		withProber(stubProber{err: errProbeFailed{}}),
		withStreams(&fakeStreams{}))
	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mp4", "bytes")

	recorder := env.do(t, http.MethodPost, "/api/entities/"+entity.ID+"/playback", "")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}

	recorder = env.do(t, http.MethodGet, "/metrics", "")
	if body := recorder.Body.String(); !strings.Contains(body, "astraeus_probe_errors_total 1") {
		t.Errorf("the probe failure was not counted:\n%s", body)
	}
}

// errProbeFailed is a distinct error type so the failure path is unambiguous.
type errProbeFailed struct{}

func (errProbeFailed) Error() string { return "ffprobe exploded" }

func TestMetricsEndpoint_EmptyWhenNoMetricsConfigured(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, func(deps *Deps) { deps.Metrics = nil })

	recorder := env.do(t, http.MethodGet, "/metrics", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("body = %q, want it empty when metrics are disabled", recorder.Body.String())
	}
}
