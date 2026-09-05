package observability

// diag.go — the observability collector's diagnostic probe.
//
// Every number here is already held by the collector: the ring's length, the
// Stats it accumulates when Metrics is on, the stream's subscriber and drop
// counts. The probe reads them. Nothing is added to Publish, which is the one
// path in this package that runs per signal.
//
// The registration is done by NewCollector rather than by whoever holds the
// handle, so a project that builds a collector and forgets to wire it into a
// dashboard is still diagnosable — which is precisely the project most likely to
// need it.

import (
	"fmt"
	"sort"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagName is the registry key. "observability" matches the feature name.
const diagName = "observability"

// RegisterDiagnostics publishes col as the process's observability diagnostic.
//
// Called by [NewCollector]. Exported for the same reason the events package
// exports its equivalent: a handle obtained from elsewhere can be made the
// reported one.
func RegisterDiagnostics(col *Collector) {
	if col == nil {
		return
	}
	diag.Register(diagName, col.probe)
}

// probe reports the collector's state.
func (c *Collector) probe() diag.Report {
	if c == nil {
		return diag.Off("no observability collector is registered")
	}
	if c.closed.Load() {
		return diag.Off("the observability collector is closed; signals are being discarded")
	}

	st := c.Stats()
	dropped := c.Dropped()

	detail := map[string]any{
		"signals":         st.Signals,
		"failed":          st.Failed,
		"cancelled":       st.Cancelled,
		"async":           st.Async,
		"children":        st.Children,
		"listener_calls":  st.Executed,
		"buffered":        c.Len(),
		"capacity":        c.cfg.Capacity,
		"distinct_names":  len(c.Names()),
		"subscribers":     c.Subscribers(),
		"dropped_to_slow": dropped,
		"metrics_enabled": c.cfg.Metrics,
		"rate_per_sec":    c.Rate(time.Minute),
		"sources":         c.sourceCounts(),
	}

	summary := fmt.Sprintf("%d signal(s) from %d name(s), %d buffered of %d",
		st.Signals, len(c.Names()), c.Len(), c.cfg.Capacity)

	var notes []string
	degraded := false
	if dropped > 0 {
		degraded = true
		notes = append(notes, fmt.Sprintf("%d signal(s) were dropped rather than delivered to a "+
			"subscriber that could not keep up. The collector never blocks a producer, so the loss "+
			"is in the live stream only — the ring buffer is unaffected.", dropped))
	}
	if !c.cfg.Metrics {
		notes = append(notes, "Per-name metrics are disabled on this collector, so Metrics() and "+
			"TopNames() return nothing. The signal counts above are still accurate.")
	}
	if c.Len() == c.cfg.Capacity && st.Signals > uint64(c.cfg.Capacity) {
		notes = append(
			notes,
			fmt.Sprintf("The ring buffer is full at %d, so the oldest signals have "+
				"been evicted. Roughly %d signal(s) are no longer readable through Snapshot.",
				c.cfg.Capacity, st.Signals-uint64(c.cfg.Capacity)),
		)
	}

	if degraded {
		return diag.Degraded(summary, detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// sourceCounts totals the buffered signals per source.
//
// Computed from the ring rather than kept as a running counter, because a running
// counter would mean another map update inside Publish and this question is only
// ever asked by a probe. The cost is one pass over a bounded buffer, at read time.
func (c *Collector) sourceCounts() map[string]int {
	counts := map[string]int{}
	for _, s := range c.ring.Snapshot() {
		src := string(s.Source)
		if src == "" {
			src = "unspecified"
		}
		counts[src]++
	}
	return counts
}

// TopSources reports the busiest sources in the buffer, most first.
//
// Exported because the dashboard's Events page and the MCP diagnostic both want
// it, and both would otherwise re-derive it from Snapshot with the same loop.
func (c *Collector) TopSources(n int) []SourceCount {
	if c == nil {
		return nil
	}

	counts := c.sourceCounts()
	out := make([]SourceCount, 0, len(counts))
	for src, count := range counts {
		out = append(out, SourceCount{Source: Source(src), Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Source < out[j].Source
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// SourceCount is one source's share of the buffer.
type SourceCount struct {
	Source Source `json:"source"`
	Count  int    `json:"count"`
}
