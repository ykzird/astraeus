// Package observability holds the metrics the media server exposes, in the
// Prometheus text exposition format.
//
// It is deliberately a small hand-written implementation rather than the
// prometheus/client_golang dependency: the project needs counters and simple
// histograms, the exposition format is stable and well specified, and keeping
// the dependency out means the metrics pipeline is ordinary code we can read
// and test. If summaries, exemplars or a push gateway are ever needed, replacing
// this package with the official client is a contained change.
package observability

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultBuckets are the histogram bucket bounds, in seconds. They span the
// range a media server actually sees: a cached metadata lookup, a direct-play
// file open, and the cold start of a transcode.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// Metrics collects counters, gauges and histograms.
//
// Every method is safe for concurrent use. Labels are supplied as a map, which
// is convenient to call and cheap enough at this scale; it is not the fastest
// possible representation, and that tradeoff is deliberate.
type Metrics struct {
	mu         sync.RWMutex
	counters   map[string]*family
	gauges     map[string]*family
	histograms map[string]*histogramFamily
	buckets    []float64
}

// sample is one labelled value within a family.
type sample struct {
	labels map[string]string
	key    string
	value  float64
}

type family struct {
	name   string
	help   string
	kind   string // "counter" or "gauge"
	series map[string]*sample
}

type histogramFamily struct {
	name    string
	help    string
	buckets []float64
	series  map[string]*histogramSeries
}

type histogramSeries struct {
	labels map[string]string
	key    string
	counts []uint64 // one per bucket, plus the implicit +Inf
	sum    float64
	total  uint64
}

// New creates an empty Metrics with the default bucket bounds.
func New() *Metrics {
	return &Metrics{
		counters:   make(map[string]*family),
		gauges:     make(map[string]*family),
		histograms: make(map[string]*histogramFamily),
		buckets:    append([]float64(nil), DefaultBuckets...),
	}
}

// IncCounter adds one to a counter series.
func (m *Metrics) IncCounter(name, help string, labels map[string]string) {
	m.AddCounter(name, help, 1, labels)
}

// AddCounter adds delta to a counter series.
func (m *Metrics) AddCounter(name, help string, delta float64, labels map[string]string) {
	if m == nil {
		return
	}
	if delta < 0 {
		// Prometheus counters are monotonic; rejecting this here keeps a bug
		// from silently producing a counter that goes backwards.
		panic("observability: counters must not decrease")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	f, ok := m.counters[name]
	if !ok {
		f = &family{name: name, help: help, kind: "counter", series: make(map[string]*sample)}
		m.counters[name] = f
	}
	key := labelKey(labels)
	s, ok := f.series[key]
	if !ok {
		s = &sample{labels: copyLabels(labels), key: key}
		f.series[key] = s
	}
	s.value += delta
}

// SetGauge sets a gauge series to an absolute value.
func (m *Metrics) SetGauge(name, help string, value float64, labels map[string]string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	f, ok := m.gauges[name]
	if !ok {
		f = &family{name: name, help: help, kind: "gauge", series: make(map[string]*sample)}
		m.gauges[name] = f
	}
	key := labelKey(labels)
	s, ok := f.series[key]
	if !ok {
		s = &sample{labels: copyLabels(labels), key: key}
		f.series[key] = s
	}
	s.value = value
}

// ObserveHistogram records one observation in a histogram series.
func (m *Metrics) ObserveHistogram(name, help string, value float64, labels map[string]string) {
	if m == nil {
		return
	}
	if math.IsNaN(value) {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	h, ok := m.histograms[name]
	if !ok {
		h = &histogramFamily{
			name:    name,
			help:    help,
			buckets: append([]float64(nil), m.buckets...),
			series:  make(map[string]*histogramSeries),
		}
		m.histograms[name] = h
	}

	key := labelKey(labels)
	s, ok := h.series[key]
	if !ok {
		s = &histogramSeries{
			labels: copyLabels(labels),
			key:    key,
			counts: make([]uint64, len(h.buckets)+1),
		}
		h.series[key] = s
	}

	s.sum += value
	s.total++
	for i, bound := range h.buckets {
		if value <= bound {
			s.counts[i]++
		}
	}
	s.counts[len(h.buckets)]++ // the +Inf bucket
}

// WriteTo renders every metric in the Prometheus text exposition format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	if m == nil {
		return 0, nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var buf strings.Builder

	writeFamily(&buf, m.counters)
	writeFamily(&buf, m.gauges)
	writeHistograms(&buf, m.histograms)

	n, err := io.WriteString(w, buf.String())
	return int64(n), err
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if _, err := m.WriteTo(w); err != nil {
			// The status line has already gone out; nothing useful is left to do.
			return
		}
	})
}

func writeFamily(buf *strings.Builder, families map[string]*family) {
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		f := families[name]
		writeMeta(buf, name, f.help, f.kind)
		for _, s := range sortedSamples(f.series) {
			buf.WriteString(name)
			writeLabels(buf, s.labels, "")
			buf.WriteByte(' ')
			buf.WriteString(formatFloat(s.value))
			buf.WriteByte('\n')
		}
	}
}

func writeHistograms(buf *strings.Builder, families map[string]*histogramFamily) {
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		h := families[name]
		writeMeta(buf, name, h.help, "histogram")

		keys := make([]string, 0, len(h.series))
		for key := range h.series {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			s := h.series[key]
			for i, bound := range h.buckets {
				buf.WriteString(name + "_bucket")
				writeLabels(buf, s.labels, formatFloat(bound))
				buf.WriteByte(' ')
				buf.WriteString(strconv.FormatUint(s.counts[i], 10))
				buf.WriteByte('\n')
			}
			buf.WriteString(name + "_bucket")
			writeLabels(buf, s.labels, "+Inf")
			buf.WriteByte(' ')
			buf.WriteString(strconv.FormatUint(s.counts[len(h.buckets)], 10))
			buf.WriteByte('\n')

			buf.WriteString(name + "_sum")
			writeLabels(buf, s.labels, "")
			buf.WriteByte(' ')
			buf.WriteString(formatFloat(s.sum))
			buf.WriteByte('\n')

			buf.WriteString(name + "_count")
			writeLabels(buf, s.labels, "")
			buf.WriteByte(' ')
			buf.WriteString(strconv.FormatUint(s.total, 10))
			buf.WriteByte('\n')
		}
	}
}

func writeMeta(buf *strings.Builder, name, help, kind string) {
	if help != "" {
		buf.WriteString("# HELP " + name + " " + escapeHelp(help) + "\n")
	}
	buf.WriteString("# TYPE " + name + " " + kind + "\n")
}

// writeLabels renders the label set, always placing "le" last as the exposition
// format expects.
func writeLabels(buf *strings.Builder, labels map[string]string, le string) {
	if len(labels) == 0 && le == "" {
		return
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(k)
		buf.WriteString(`="`)
		buf.WriteString(escapeLabel(labels[k]))
		buf.WriteByte('"')
	}
	if le != "" {
		if len(keys) > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`le="` + le + `"`)
	}
	buf.WriteByte('}')
}

func sortedSamples(series map[string]*sample) []*sample {
	keys := make([]string, 0, len(series))
	for key := range series {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]*sample, 0, len(keys))
	for _, key := range keys {
		out = append(out, series[key])
	}
	return out
}

// labelKey builds a stable key for a label set, so samples with the same labels
// share a series regardless of map ordering.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(labels[k])
		buf.WriteByte(0)
	}
	return buf.String()
}

func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// formatFloat renders a value the way Prometheus expects.
func formatFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeHelp escapes a HELP line, where only backslashes and newlines are
// special.
func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// escapeLabel escapes a label value: backslash, double quote and newline.
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// Render is a convenience wrapper used by tests and debugging.
func (m *Metrics) Render() string {
	var buf strings.Builder
	if _, err := m.WriteTo(&buf); err != nil {
		return fmt.Sprintf("rendering metrics failed: %v", err)
	}
	return buf.String()
}
