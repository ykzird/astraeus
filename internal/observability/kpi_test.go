package observability

import (
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
