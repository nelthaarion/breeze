package video

// diag.go — the video mount's diagnostic probe.
//
// Video was diagnosable through the dashboard's Video page, but only when an
// event bus was attached *and* the collector's video tracker had been attached to
// it. A mount configured with no bus — which is the default — served terabytes
// invisibly. This probe closes that: it answers from the mount's own counters and
// its resolved configuration, with no bus, no dashboard and no event subscription
// required.
//
// Registration happens in newMount, so both Mount and Handler get it. A process
// with several mounts reports the one registered last under "video" and every one
// of them under "video:<prefix>", because a multi-mount process asking "how much
// am I serving" needs the per-mount answer and a single-mount process should not
// have to know that.

import (
	"fmt"
	"sort"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagName is the registry key, matching the `breeze add video` feature name.
const diagName = "video"

// registerDiagnostics publishes m under the shared key and its own prefixed one.
//
// Unexported: a mount is only ever created by this package, so there is no
// caller outside it that could hold one to register.
func (m *mount) registerDiagnostics() {
	diag.Register(diagName, m.probe)
	diag.Register(diagName+":"+m.prefix, m.probe)
}

// probe reports the mount's state.
func (m *mount) probe() diag.Report {
	if m == nil {
		return diag.Off("no video mount is registered")
	}

	served := m.served.Load()
	failed := m.failedReqs.Load()
	bytes := m.bytesSent.Load()

	detail := map[string]any{
		"prefix":       m.prefix,
		"root":         m.root,
		"extensions":   m.extensionList(),
		"requests":     served,
		"partial":      m.partial.Load(),
		"failed":       failed,
		"disconnects":  m.disconnects.Load(),
		"bytes_sent":   bytes,
		"chunk_bytes":  m.chunk,
		"signed_urls":  len(m.secret) > 0,
		"opaque_names": m.opaque,
		"authorizer":   m.authorize != nil,
		"bus_attached": m.bus != nil,
		"collector":    m.col != nil,
		"hidden_files": m.allowHid,
		"follow_links": m.followLink,
	}
	if m.maxChunk > 0 {
		detail["max_chunk_bytes"] = m.maxChunk
	}
	if m.cache != "" {
		detail["cache_control"] = m.cache
	}
	if m.anyOrigin {
		detail["cors"] = "any origin"
	} else if len(m.origins) > 0 {
		detail["cors"] = fmt.Sprintf("%d allowed origin(s)", len(m.origins))
	}
	if nanos := m.lastServedNs.Load(); nanos != 0 {
		detail["last_request"] = time.Unix(0, nanos).UTC().Format(time.RFC3339Nano)
	}

	summary := fmt.Sprintf("%s from %s: %d request(s), %s sent",
		m.prefix, m.root, served, diag.HumanBytes(bytes))

	// Two configuration facts are worth saying out loud, because each is a
	// deliberate loosening whose consequence is not visible from the outside.
	var notes []string
	if len(m.secret) == 0 && m.authorize == nil {
		notes = append(notes, "This mount has neither a signing secret nor an Authorize hook, so "+
			"every file under the root is publicly readable by anyone who can guess its name.")
	}
	if m.followLink {
		notes = append(notes, "FollowSymlinks is on, so a symlink inside the root can serve a file "+
			"outside it.")
	}
	if m.bus == nil && m.col == nil {
		notes = append(notes, "No event bus or observability collector is attached, so this mount's "+
			"activity does not reach the dashboard's Video page. The counters above are unaffected.")
	}

	// A failure rate above a tenth of requests is a mount that is not working —
	// a missing root, a wrong extension list, a signing secret that does not
	// match what is signing the URLs.
	if served > 0 && failed*10 > served {
		return diag.Degraded(fmt.Sprintf("%s — %d of %d request(s) failed", summary, failed, served),
			detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// extensionList renders the allowed extensions, sorted, for a report.
//
// The mount stores them as a set for O(1) lookup on the request path; a report
// needs them ordered so two reads of an unchanged mount produce the same output.
func (m *mount) extensionList() []string {
	out := make([]string, 0, len(m.exts))
	for e := range m.exts {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
