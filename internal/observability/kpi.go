package observability

// This file is the single declaration of the project's KPI registry, matching
// section 6.1 of the specification. Every metric the server emits is declared
// here, so an idle server still exposes the full registry: a scraper can see
// the metric exists and alert on absence, instead of reporting "no data" until
// the first event happens to occur.
//
// Declaration is authoritative. Observation sites pass their own help text,
// which is ignored when the metric was already declared at startup.

// Metric names.
const (
	// MetricFirstSegment is the specification's fttt_latency: the wait before
	// the first segment reaches a client.
	MetricFirstSegment = "astraeus_first_segment_seconds"
	// MetricTranscodeStartup is transcode_startup_time.
	MetricTranscodeStartup = "astraeus_transcode_startup_seconds"
	// MetricMetadataLookup is metadata_latency.
	MetricMetadataLookup = "astraeus_metadata_lookup_seconds"
	// MetricStreamErrors over MetricStreamSessions is stream_error_rate.
	MetricStreamErrors   = "astraeus_stream_errors_total"
	MetricStreamSessions = "astraeus_stream_sessions_total"

	MetricStreamSessionsActive = "astraeus_stream_sessions_active"
	MetricPlaybackDecisions    = "astraeus_playback_decisions_total"
	MetricProbeErrors          = "astraeus_probe_errors_total"
	MetricScanSeconds          = "astraeus_scan_seconds"
	MetricScanFiles            = "astraeus_scan_files_total"
	MetricScanRuns             = "astraeus_scan_runs_total"
	MetricHTTPRequests         = "astraeus_http_requests_total"
	MetricHTTPRequestSeconds   = "astraeus_http_request_seconds"
	MetricTranscodeFallbacks   = "astraeus_transcode_fallbacks_total"
	MetricAuthGranted          = "astraeus_auth_granted_total"
	MetricAuthDenied           = "astraeus_auth_denied_total"
	MetricRateLimited          = "astraeus_rate_limited_total"
)

// KPIDefinition describes one entry in the registry.
type KPIDefinition struct {
	Name string
	Help string
	Kind string // "counter", "gauge" or "histogram"
}

// KPIRegistry is every metric this server emits.
var KPIRegistry = []KPIDefinition{
	{
		Name: MetricFirstSegment,
		Help: "Time from stream preparation starting to the first segment being delivered to a client (the specification's fttt_latency).",
		Kind: "histogram",
	},
	{
		Name: MetricTranscodeStartup,
		Help: "Time from starting ffmpeg to the playlist being available (transcode_startup_time).",
		Kind: "histogram",
	},
	{
		Name: MetricMetadataLookup,
		Help: "Time taken to retrieve and process a MetadataSet for one entity (metadata_latency).",
		Kind: "histogram",
	},
	{
		Name: MetricStreamErrors,
		Help: "Segmented streaming failures. Divided by " + MetricStreamSessions + " this is stream_error_rate.",
		Kind: "counter",
	},
	{
		Name: MetricStreamSessions,
		Help: "Segmented streaming sessions, by mode and outcome.",
		Kind: "counter",
	},
	{
		Name: MetricStreamSessionsActive,
		Help: "Segmented streaming sessions currently running.",
		Kind: "gauge",
	},
	{
		Name: MetricPlaybackDecisions,
		Help: "Playback negotiations, by the mode they chose.",
		Kind: "counter",
	},
	{
		Name: MetricProbeErrors,
		Help: "Media probes that failed.",
		Kind: "counter",
	},
	{
		Name: MetricScanSeconds,
		Help: "Time taken to walk one library directory and reconcile it with the database.",
		Kind: "histogram",
	},
	{
		Name: MetricScanFiles,
		Help: "Media files seen by the scanner.",
		Kind: "counter",
	},
	{
		Name: MetricScanRuns,
		Help: "Completed scan passes per library.",
		Kind: "counter",
	},
	{
		Name: MetricHTTPRequests,
		Help: "HTTP requests served, by method and status code.",
		Kind: "counter",
	},
	{
		Name: MetricHTTPRequestSeconds,
		Help: "HTTP request duration.",
		Kind: "histogram",
	},
	{
		Name: MetricTranscodeFallbacks,
		Help: "Transcodes that failed on a hardware encoder and were retried in software.",
		Kind: "counter",
	},
	{
		Name: MetricAuthGranted,
		Help: "Requests admitted by the access gate.",
		Kind: "counter",
	},
	{
		Name: MetricAuthDenied,
		Help: "Requests refused by the access gate, by reason.",
		Kind: "counter",
	},
	{
		Name: MetricRateLimited,
		Help: "API requests refused by the rate limiter. A sustained non-zero rate means a client is over its allowance.",
		Kind: "counter",
	},
}

// DeclareKPIs registers every KPI in the registry. Call it once at startup so
// the metrics endpoint exposes the registry before anything has happened.
func DeclareKPIs(metrics *Metrics) {
	if metrics == nil {
		return
	}
	for _, definition := range KPIRegistry {
		metrics.declare(definition)
	}
}

// declare creates an empty family so its HELP and TYPE lines are exposed even
// with no series yet.
func (m *Metrics) declare(definition KPIDefinition) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch definition.Kind {
	case "counter":
		if _, ok := m.counters[definition.Name]; !ok {
			m.counters[definition.Name] = &family{
				name:   definition.Name,
				help:   definition.Help,
				kind:   "counter",
				series: make(map[string]*sample),
			}
		}
	case "gauge":
		if _, ok := m.gauges[definition.Name]; !ok {
			m.gauges[definition.Name] = &family{
				name:   definition.Name,
				help:   definition.Help,
				kind:   "gauge",
				series: make(map[string]*sample),
			}
		}
	case "histogram":
		if _, ok := m.histograms[definition.Name]; !ok {
			m.histograms[definition.Name] = &histogramFamily{
				name:    definition.Name,
				help:    definition.Help,
				buckets: append([]float64(nil), m.buckets...),
				series:  make(map[string]*histogramSeries),
			}
		}
	}
}
