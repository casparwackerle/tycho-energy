// file: internal/tycho/analysis/export/prometheus.go
package analysisexport

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"
)

type PrometheusConfig struct {
	// Prefix for exported metric names. If empty, "tycho_" is used.
	// If set to "tycho", it will become "tycho_".
	Prefix string

	// Export debug/quality info as additional gauges.
	EnableDebug bool
}

// PrometheusSink consumes analysis.Point (push) and exposes them via a Prometheus collector (pull).
//
// Key property: this registers into an existing prometheus.Registry (the one already used by Kepler),
// and therefore reuses Kepler's HTTP server as-is.
type PrometheusSink struct {
	cfg PrometheusConfig

	mu       sync.RWMutex
	families map[analysis.MetricID]*promFamily

	collector prometheus.Collector
}

type promFamily struct {
	// Prometheus metric base name (already prefixed and sanitized)
	name string

	// First-seen unit for help string (informational only)
	unit string

	// First-seen label schema (sorted keys), fixed per family
	labelKeys []string

	// series key -> last sample
	series map[string]*promSeries

	// shared Descs for main + debug
	descValue       *prometheus.Desc
	descWinStart    *prometheus.Desc
	descWinEnd      *prometheus.Desc
	descSamplesUsed *prometheus.Desc
	descSocketsUsed *prometheus.Desc
	descDelayTicks  *prometheus.Desc

	// Helpful warning gate: log schema mismatch once
	warnedSchema bool
}

type promSeries struct {
	labelVals []string

	value float64

	winStart uint64
	winEnd   uint64

	hasQuality bool
	samples    int
	sockets    int
	delayTicks uint64
}

func NewPrometheusSink(cfg PrometheusConfig) *PrometheusSink {
	if cfg.Prefix == "" {
		cfg.Prefix = "tycho_"
	} else if !strings.HasSuffix(cfg.Prefix, "_") {
		cfg.Prefix = cfg.Prefix + "_"
	}

	s := &PrometheusSink{
		cfg:      cfg,
		families: make(map[analysis.MetricID]*promFamily, 64),
	}
	s.collector = &tychoAnalysisCollector{sink: s}
	return s
}

// Collector returns a prometheus.Collector that can be registered into an existing registry.
func (s *PrometheusSink) Collector() prometheus.Collector { return s.collector }

// Emit implements analysis.Sink. It updates the latest value for a time-series.
func (s *PrometheusSink) Emit(_ context.Context, p analysis.Point) {
	if s == nil {
		return
	}

	// Defensive copy: labels map is mutable.
	labels := p.Key.Labels.Clone()

	s.mu.Lock()
	defer s.mu.Unlock()

	f := s.ensureFamilyLocked(p.Key.ID, labels, p.Unit)

	// Build label values in fixed order (missing keys -> "")
	lv := make([]string, len(f.labelKeys))
	for i, k := range f.labelKeys {
		lv[i] = labels[k]
	}

	// Construct a stable per-series key under the fixed schema.
	// This avoids issues where CanonicalString() would differ when some labels are missing.
	serKey := seriesKey(f.name, f.labelKeys, lv)

	ps := f.series[serKey]
	if ps == nil {
		ps = &promSeries{labelVals: lv}
		f.series[serKey] = ps
	} else {
		// Keep stored slice immutable to reduce accidental aliasing.
		ps.labelVals = lv
	}

	ps.value = p.Value
	ps.winStart = p.Window.StartMono
	ps.winEnd = p.Window.EndMono

	if p.Quality != nil {
		ps.hasQuality = true
		ps.samples = p.Quality.SamplesUsed
		ps.sockets = p.Quality.SocketsUsed
		ps.delayTicks = p.Quality.DelayTicks
	}
}

// --- Collector implementation ---

type tychoAnalysisCollector struct {
	sink *PrometheusSink
}

func (c *tychoAnalysisCollector) Describe(ch chan<- *prometheus.Desc) {
	// Intentionally empty.
	// This collector has dynamic metrics, so we describe nothing
	// and let Collect provide the descriptors implicitly.
}

func (c *tychoAnalysisCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.sink
	if s == nil {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, f := range s.families {
		for _, ps := range f.series {
			// main value
			ch <- prometheus.MustNewConstMetric(
				f.descValue,
				prometheus.GaugeValue,
				ps.value,
				ps.labelVals...,
			)

			if !s.cfg.EnableDebug {
				continue
			}

			// window ticks
			if f.descWinStart != nil {
				ch <- prometheus.MustNewConstMetric(
					f.descWinStart,
					prometheus.GaugeValue,
					float64(ps.winStart),
					ps.labelVals...,
				)
			}
			if f.descWinEnd != nil {
				ch <- prometheus.MustNewConstMetric(
					f.descWinEnd,
					prometheus.GaugeValue,
					float64(ps.winEnd),
					ps.labelVals...,
				)
			}

			// quality (emit 0 if never provided; helps spot missing Quality fields)
			if f.descSamplesUsed != nil {
				v := 0.0
				if ps.hasQuality {
					v = float64(ps.samples)
				}
				ch <- prometheus.MustNewConstMetric(
					f.descSamplesUsed,
					prometheus.GaugeValue,
					v,
					ps.labelVals...,
				)
			}
			if f.descSocketsUsed != nil {
				v := 0.0
				if ps.hasQuality {
					v = float64(ps.sockets)
				}
				ch <- prometheus.MustNewConstMetric(
					f.descSocketsUsed,
					prometheus.GaugeValue,
					v,
					ps.labelVals...,
				)
			}
			if f.descDelayTicks != nil {
				v := 0.0
				if ps.hasQuality {
					v = float64(ps.delayTicks)
				}
				ch <- prometheus.MustNewConstMetric(
					f.descDelayTicks,
					prometheus.GaugeValue,
					v,
					ps.labelVals...,
				)
			}
		}
	}
}

// --- family/schema management (locked) ---

func (s *PrometheusSink) ensureFamilyLocked(id analysis.MetricID, labels analysis.Labels, unit string) *promFamily {
	f := s.families[id]
	if f != nil {
		// Enforce Prometheus rule: stable label keys per metric name.
		// If new keys appear later, ignore them (and warn once).
		if !f.warnedSchema && hasNewLabelKeys(labels, f.labelKeys) {
			f.warnedSchema = true
			klog.Warningf("[analysis/prom] metric id=%q emitted with new label keys after first sight; schema fixed to %v; extra keys will be ignored",
				id, f.labelKeys)
		}
		return f
	}

	labelKeys := sortedKeys(labels)
	name := s.cfg.Prefix + sanitizePromName(string(id))
	if name == s.cfg.Prefix || !isValidPromName(name) {
		// Last resort: still provide something valid.
		name = s.cfg.Prefix + "metric"
	}

	help := fmt.Sprintf("Tycho analysis metric %s (unit: %s).", id, unit)

	f = &promFamily{
		name:      name,
		unit:      unit,
		labelKeys: labelKeys,
		series:    make(map[string]*promSeries, 32),
		descValue: prometheus.NewDesc(name, help, labelKeys, nil),
	}

	if s.cfg.EnableDebug {
		f.descWinStart = prometheus.NewDesc(name+"_window_start_mono", "Analysis window start (monotonic ticks).", labelKeys, nil)
		f.descWinEnd = prometheus.NewDesc(name+"_window_end_mono", "Analysis window end (monotonic ticks).", labelKeys, nil)

		f.descSamplesUsed = prometheus.NewDesc(name+"_samples_used", "Number of raw samples contributing to this point (0 if not provided).", labelKeys, nil)
		f.descSocketsUsed = prometheus.NewDesc(name+"_sockets_used", "Number of sockets contributing to this point (0 if not provided).", labelKeys, nil)
		f.descDelayTicks = prometheus.NewDesc(name+"_delay_ticks", "Effective delay applied when selecting the window (ticks, 0 if not provided).", labelKeys, nil)
	}

	s.families[id] = f
	klog.Infof("[analysis/prom] discovered metric id=%q -> %q labels=%v unit=%q", id, name, labelKeys, unit)
	return f
}

func hasNewLabelKeys(labels map[string]string, schema []string) bool {
	if len(labels) == 0 {
		return false
	}
	if len(schema) == 0 {
		return true
	}
	allowed := make(map[string]struct{}, len(schema))
	for _, k := range schema {
		allowed[k] = struct{}{}
	}
	for k := range labels {
		if _, ok := allowed[k]; !ok {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func seriesKey(name string, keys []string, vals []string) string {
	// name|k1=v1|k2=v2...
	var b strings.Builder
	b.WriteString(name)
	for i := range keys {
		b.WriteByte('|')
		b.WriteString(keys[i])
		b.WriteByte('=')
		b.WriteString(vals[i])
	}
	return b.String()
}

// --- Prometheus name sanitization ---

var promNameRe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func sanitizePromName(s string) string {
	s = strings.TrimSpace(s)
	s = promNameRe.ReplaceAllString(s, "_")
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	s = strings.Trim(s, "_")
	if s == "" {
		return ""
	}
	// Must not start with a digit.
	if s[0] >= '0' && s[0] <= '9' {
		s = "m_" + s
	}
	return s
}

func isValidPromName(name string) bool {
	if name == "" {
		return false
	}
	c0 := name[0]
	if !((c0 >= 'a' && c0 <= 'z') || (c0 >= 'A' && c0 <= 'Z') || c0 == '_') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}
