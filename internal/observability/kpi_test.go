package observability

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func TestDeclareKPIs_ExposesRegistryBeforeAnyEvent(t *testing.T) {
	t.Parallel()

	metrics := New()
	DeclareKPIs(metrics)

	rendered := metrics.Render()
	for _, definition := range KPIRegistry {
		want := "# TYPE " + definition.Name + " " + definition.Kind
		if !strings.Contains(rendered, want) {
			t.Errorf("idle metrics are missing %q\n---\n%s", want, rendered)
		}
		if !strings.Contains(rendered, "# HELP "+definition.Name+" ") {
			t.Errorf("idle metrics are missing the HELP line for %s", definition.Name)
		}
	}
}

func TestDeclareKPIs_CoversTheSpecifiedKPIregistry(t *testing.T) {
	t.Parallel()

	// The four metrics the specification names in section 6.1 must all be
	// present in the registry.
	required := map[string]string{
		MetricFirstSegment:     "histogram",
		MetricTranscodeStartup: "histogram",
		MetricMetadataLookup:   "histogram",
		MetricStreamErrors:     "counter",
	}

	byName := make(map[string]KPIDefinition, len(KPIRegistry))
	for _, definition := range KPIRegistry {
		byName[definition.Name] = definition
		if definition.Help == "" {
			t.Errorf("%s has no help text", definition.Name)
		}
		if !strings.HasPrefix(definition.Name, "astraeus_") {
			t.Errorf("%s is not namespaced", definition.Name)
		}
	}

	for name, kind := range required {
		definition, ok := byName[name]
		if !ok {
			t.Errorf("the specification's KPI %s is not in the registry", name)
			continue
		}
		if definition.Kind != kind {
			t.Errorf("%s kind = %q, want %q", name, definition.Kind, kind)
		}
	}
}

func TestDeclareKPIs_DoesNotClobberSamples(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.IncCounter(MetricStreamErrors, "ignored", map[string]string{"mode": "remux"})
	DeclareKPIs(metrics)

	if rendered := metrics.Render(); !strings.Contains(rendered, `astraeus_stream_errors_total{mode="remux"} 1`) {
		t.Errorf("declaring the registry dropped an existing sample:\n%s", rendered)
	}
}

func TestDeclareKPIs_IsIdempotent(t *testing.T) {
	t.Parallel()

	metrics := New()
	DeclareKPIs(metrics)
	DeclareKPIs(metrics)

	rendered := metrics.Render()
	if got := strings.Count(rendered, "# TYPE "+MetricStreamErrors+" "); got != 1 {
		t.Errorf("TYPE line appears %d times, want 1:\n%s", got, rendered)
	}
}

func TestDeclareKPIs_NilMetricsIsSafe(t *testing.T) {
	t.Parallel()

	DeclareKPIs(nil)
}

// quantileFromFamily interpolates a quantile from a histogram family's
// cumulative counts, which is what a Prometheus query does with these buckets.
// It reads the family directly so the test is about the bucket bounds rather
// than about the exposition format.
func quantileFromFamily(t *testing.T, m *Metrics, name string, q float64) float64 {
	t.Helper()

	m.mu.RLock()
	defer m.mu.RUnlock()

	h, ok := m.histograms[name]
	if !ok {
		t.Fatalf("no histogram %q", name)
	}
	var s *histogramSeries
	for _, candidate := range h.series {
		s = candidate
	}
	if s == nil || s.total == 0 {
		t.Fatalf("no observations in %q", name)
	}

	rank := q * float64(s.total)
	lowerBound := 0.0
	lowerCount := 0.0
	for i, bound := range h.buckets {
		cumulative := float64(s.counts[i])
		if cumulative >= rank {
			inBucket := cumulative - lowerCount
			fraction := 0.0
			if inBucket > 0 {
				fraction = (rank - lowerCount) / inBucket
			}
			return lowerBound + fraction*(bound-lowerBound)
		}
		lowerBound = bound
		lowerCount = cumulative
	}
	return h.buckets[len(h.buckets)-1]
}

// The four transcode startups the round-14 load run measured against the 4K
// film, in seconds. With one bucket list for every histogram these all landed
// in the (5s, 10s] bin, so the p50 came back as 7.50s against a true 6.10s:
// interpolating across a five-second gap is not a measurement.
func TestTranscodeStartup_ResolvesPercentilesFinelyEnoughToMeasure(t *testing.T) {
	t.Parallel()

	metrics := New()
	DeclareKPIs(metrics)
	for _, startup := range []float64{5.85, 6.00, 6.20, 6.35} {
		metrics.ObserveHistogram(MetricTranscodeStartup, "startup", startup, nil)
	}

	got := quantileFromFamily(t, metrics, MetricTranscodeStartup, 0.5)
	if math.Abs(got-6.10) > 0.5 {
		t.Errorf("p50 = %.2fs, want within 0.5s of 6.10s: the bucket bounds are too coarse to measure a transcode startup", got)
	}
}

// The same for the 1080p HEVC case, where the client measured a p50 of 2.87s
// across eight concurrent streams and the histogram reported 3.75s.
func TestFirstSegment_ResolvesPercentilesFinelyEnoughToMeasure(t *testing.T) {
	t.Parallel()

	metrics := New()
	DeclareKPIs(metrics)
	for _, fttt := range []float64{2.50, 2.70, 2.75, 2.80, 2.90, 2.95, 3.05, 3.15} {
		metrics.ObserveHistogram(MetricFirstSegment, "fttt", fttt, nil)
	}

	got := quantileFromFamily(t, metrics, MetricFirstSegment, 0.5)
	if math.Abs(got-2.87) > 0.5 {
		t.Errorf("p50 = %.2fs, want within 0.5s of 2.87s: the bucket bounds are too coarse to measure first-segment latency", got)
	}
}

// The override has to hold whichever happens first. A histogram created by its
// first observation must already carry the metric's own bounds, because
// DeclareKPIs only creates a family that is not there yet.
func TestTranscodeBuckets_ApplyWithoutDeclareFirst(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.ObserveHistogram(MetricTranscodeStartup, "startup", 6.1, nil)

	got := quantileFromFamily(t, metrics, MetricTranscodeStartup, 0.5)
	if math.Abs(got-6.1) > 0.5 {
		t.Errorf("p50 = %.2fs before DeclareKPIs ran, want within 0.5s of 6.10s", got)
	}
}

// The finer bounds belong to the transcode KPIs and not to every histogram: a
// metadata lookup or an HTTP request is served by different code with a
// different shape, and widening the default list for all of them would trade
// one blind spot for another.
func TestHTTPHistograms_KeepTheDefaultBuckets(t *testing.T) {
	t.Parallel()

	metrics := New()
	DeclareKPIs(metrics)

	metrics.mu.RLock()
	got := append([]float64(nil), metrics.histograms[MetricHTTPRequestSeconds].buckets...)
	metrics.mu.RUnlock()

	if !slices.Equal(got, DefaultBuckets) {
		t.Errorf("HTTP request buckets = %v, want the defaults %v", got, DefaultBuckets)
	}
}
