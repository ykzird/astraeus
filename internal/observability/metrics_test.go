package observability

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestMetrics_Counter(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.IncCounter("astraeus_http_requests_total", "Requests served.", map[string]string{"method": "GET"})
	metrics.IncCounter("astraeus_http_requests_total", "Requests served.", map[string]string{"method": "GET"})
	metrics.AddCounter("astraeus_http_requests_total", "Requests served.", 3, map[string]string{"method": "POST"})

	got := metrics.Render()
	want := `# HELP astraeus_http_requests_total Requests served.
# TYPE astraeus_http_requests_total counter
astraeus_http_requests_total{method="GET"} 2
astraeus_http_requests_total{method="POST"} 3
`
	if got != want {
		t.Errorf("rendered metrics:\n%s\nwant:\n%s", got, want)
	}
}

func TestMetrics_UnlabelledCounter(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.IncCounter("astraeus_stream_errors_total", "Stream failures.", nil)

	got := metrics.Render()
	if !strings.Contains(got, "astraeus_stream_errors_total 1\n") {
		t.Errorf("unlabelled counter rendered incorrectly:\n%s", got)
	}
	if strings.Contains(got, `astraeus_stream_errors_total{`) {
		t.Errorf("unlabelled counter must not render an empty label set:\n%s", got)
	}
}

func TestMetrics_Gauge(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.SetGauge("astraeus_entities", "Entities known.", 42, nil)
	metrics.SetGauge("astraeus_entities", "Entities known.", 7, nil)

	got := metrics.Render()
	if !strings.Contains(got, "# TYPE astraeus_entities gauge\n") {
		t.Errorf("gauge type line missing:\n%s", got)
	}
	if !strings.Contains(got, "astraeus_entities 7\n") {
		t.Errorf("gauge should hold the last value set:\n%s", got)
	}
	if strings.Contains(got, "astraeus_entities 42") {
		t.Errorf("gauge kept a stale value:\n%s", got)
	}
}

func TestMetrics_Histogram(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.buckets = []float64{1, 5}

	metrics.ObserveHistogram("astraeus_metadata_lookup_seconds", "Metadata lookups.", 0.5, map[string]string{"provider": "tmdb"})
	metrics.ObserveHistogram("astraeus_metadata_lookup_seconds", "Metadata lookups.", 2, map[string]string{"provider": "tmdb"})
	metrics.ObserveHistogram("astraeus_metadata_lookup_seconds", "Metadata lookups.", 10, map[string]string{"provider": "tmdb"})

	got := metrics.Render()
	want := `# HELP astraeus_metadata_lookup_seconds Metadata lookups.
# TYPE astraeus_metadata_lookup_seconds histogram
astraeus_metadata_lookup_seconds_bucket{provider="tmdb",le="1"} 1
astraeus_metadata_lookup_seconds_bucket{provider="tmdb",le="5"} 2
astraeus_metadata_lookup_seconds_bucket{provider="tmdb",le="+Inf"} 3
astraeus_metadata_lookup_seconds_sum{provider="tmdb"} 12.5
astraeus_metadata_lookup_seconds_count{provider="tmdb"} 3
`
	if got != want {
		t.Errorf("rendered histogram:\n%s\nwant:\n%s", got, want)
	}
}

func TestMetrics_HistogramBucketsAreCumulative(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.buckets = []float64{1, 2, 3}

	for i := 0; i < 4; i++ {
		metrics.ObserveHistogram("x_seconds", "x", float64(i), nil)
	}

	got := metrics.Render()
	for _, want := range []string{
		`x_seconds_bucket{le="1"} 2`,
		`x_seconds_bucket{le="2"} 3`,
		`x_seconds_bucket{le="3"} 4`,
		`x_seconds_bucket{le="+Inf"} 4`,
		`x_seconds_count 4`,
		`x_seconds_sum 6`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered histogram is missing %q:\n%s", want, got)
		}
	}
}

func TestMetrics_LabelOrderDoesNotSplitSeries(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.IncCounter("c_total", "c", map[string]string{"a": "1", "b": "2"})
	metrics.IncCounter("c_total", "c", map[string]string{"b": "2", "a": "1"})

	got := metrics.Render()
	if !strings.Contains(got, `c_total{a="1",b="2"} 2`) {
		t.Errorf("labels supplied in a different order created a second series:\n%s", got)
	}
	if strings.Count(got, "c_total{") != 1 {
		t.Errorf("expected exactly one series:\n%s", got)
	}
}

func TestMetrics_EscapesLabelValues(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.IncCounter("c_total", "c", map[string]string{"path": "a\"b\\c\nd"})

	got := metrics.Render()
	if !strings.Contains(got, `c_total{path="a\"b\\c\nd"} 1`) {
		t.Errorf("label value was not escaped correctly:\n%s", got)
	}
}

func TestMetrics_EscapesHelpText(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.IncCounter("c_total", "line one\nline two \\ end", nil)

	got := metrics.Render()
	if !strings.Contains(got, `# HELP c_total line one\nline two \\ end`) {
		t.Errorf("help text was not escaped correctly:\n%s", got)
	}
}

func TestMetrics_HistogramIgnoresNaN(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.ObserveHistogram("x_seconds", "x", math.NaN(), nil)

	if got := metrics.Render(); strings.Contains(got, "x_seconds") {
		t.Errorf("a NaN observation should be dropped:\n%s", got)
	}
}

func TestMetrics_CounterMustNotDecrease(t *testing.T) {
	t.Parallel()

	metrics := New()

	defer func() {
		if recover() == nil {
			t.Error("a negative counter delta should panic rather than corrupt the metric")
		}
	}()
	metrics.AddCounter("c_total", "c", -1, nil)
}

func TestMetrics_Handler(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.IncCounter("c_total", "c", nil)

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content type = %q, want text/plain", got)
	}
	if !strings.Contains(recorder.Body.String(), "c_total 1") {
		t.Errorf("body does not contain the metric:\n%s", recorder.Body.String())
	}
}

func TestMetrics_ConcurrentUse(t *testing.T) {
	t.Parallel()

	metrics := New()

	const goroutines = 16
	const iterations = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				metrics.IncCounter("c_total", "c", map[string]string{"g": "shared"})
				metrics.ObserveHistogram("h_seconds", "h", 0.01, map[string]string{"g": "shared"})
				metrics.SetGauge("g_value", "g", float64(i), nil)
			}
		}()
	}
	wg.Wait()

	wantCount := goroutines * iterations
	got := metrics.Render()
	if !strings.Contains(got, "c_total{g=\"shared\"} "+strconv.Itoa(wantCount)) {
		t.Errorf("counter lost increments under concurrency:\n%s", got)
	}
	if !strings.Contains(got, "h_seconds_count{g=\"shared\"} "+strconv.Itoa(wantCount)) {
		t.Errorf("histogram lost observations under concurrency:\n%s", got)
	}
}

func TestMetrics_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()

	// A nil *Metrics is a valid "metrics disabled" value, so callers never need
	// to guard every observation.
	var metrics *Metrics
	metrics.IncCounter("c_total", "c", nil)
	metrics.AddCounter("c_total", "c", 1, nil)
	metrics.SetGauge("g", "g", 1, nil)
	metrics.ObserveHistogram("h", "h", 1, nil)

	if n, err := metrics.WriteTo(&strings.Builder{}); n != 0 || err != nil {
		t.Errorf("nil metrics WriteTo = (%d, %v), want (0, nil)", n, err)
	}
}

func TestFormatFloat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   float64
		want string
	}{
		{in: 0, want: "0"},
		{in: 1, want: "1"},
		{in: 42, want: "42"},
		{in: 0.5, want: "0.5"},
		{in: 12.5, want: "12.5"},
		{in: 0.0001, want: "0.0001"},
	}

	for _, tt := range tests {
		if got := formatFloat(tt.in); got != tt.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
