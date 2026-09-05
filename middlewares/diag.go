package middleware

// diag.go — diagnostics for the middleware layer.
//
// # The problem
//
// These middlewares hold no state worth reporting and, more importantly, hold no
// counts. Compression does not know how many responses it compressed; the ETag
// cache knows its store size but not its 304 rate; the rate limiter knows its
// client map but not how many requests it rejected. Those are the questions
// someone actually asks — "is compression working?", "is my ETag ever hitting?",
// "am I rate limiting anyone?" — and none of them could be answered at all.
//
// # Why the counters are gated
//
// Unlike the workflow engine or the video mount, these run on the *response* path
// of every single request, and several of them are the cheapest middleware in the
// framework: the compression middleware's early return for a client that sent no
// Accept-Encoding is two string reads. An unconditional atomic increment there
// would be a measurable fraction of the middleware's own cost, and — worse — a
// single shared counter incremented by every core moves a cache line between
// cores on every request.
//
// So these use [diag.Counter], which reads a process-wide gate before touching
// anything. With counting off the added cost is one load of a global that is in
// every core's cache in shared state, plus a perfectly-predicted branch. With
// counting on it is one atomic add on a path that has already serialised JSON or
// compressed a body.
//
// dashboard.Install and mcp.ServeInProcess both enable counting, so a project
// with either has these numbers without doing anything. A project with neither
// pays nothing and its probes say so, rather than reporting zeroes that look like
// an idle service.
//
// # Why the counters are package-level
//
// A middleware is a closure returned by a constructor, and an application may
// install two of the same kind — a strict rate limiter on /api and a loose one
// everywhere else. Per-instance counters would need per-instance registry keys,
// and a closure has no name to build one from. The counts are therefore
// per-process and per-kind, which is the granularity the question is asked at:
// "is compression working" is not a per-instance question.
//
// The exception is ETagCache, which is a real named type an application holds, so
// its probe reads that instance's store as well as the shared counter.

import (
	"fmt"

	"github.com/nelthaarion/breeze/v2/diag"
)

// Diagnostic registry keys, matching the `breeze add` feature names.
const (
	diagCompression = "compression"
	diagETag        = "etag"
	diagRateLimit   = "ratelimit"
	diagCORS        = "cors"
	diagSecurity    = "security"
	diagJWT         = "jwt"
	diagLocale      = "locale"
	diagLogging     = "logging"
	diagRecovery    = "recovery"
)

// Shared counters, one per middleware kind.
//
// Package-level values rather than pointers: a diag.Counter's zero value is ready
// to use, so there is nothing to construct and no nil to guard.
var (
	compressionCounter diag.Counter
	etagCounter        diag.Counter
	rateLimitCounter   diag.Counter
	corsCounter        diag.Counter
	securityCounter    diag.Counter
	jwtCounter         diag.Counter
	localeCounter      diag.Counter
	loggingCounter     diag.Counter
	recoveryCounter    diag.Counter
)

// counterDetail folds a snapshot into a Report's Detail, with the labels this
// middleware's hits and misses actually mean.
//
// Passing the two labels rather than reusing "hits" and "misses" everywhere is
// the difference between a report that can be read and one that has to be
// decoded: "compressed: 412, passed_through: 88" needs no explanation, and
// "hits: 412, misses: 88" needs the source.
func counterDetail(s diag.CounterSnapshot, hitLabel, missLabel string) map[string]any {
	detail := map[string]any{
		hitLabel:   s.Hits,
		missLabel:  s.Misses,
		"counting": s.Counting,
	}
	if s.Errors > 0 {
		detail["errors"] = s.Errors
	}
	if s.Bytes > 0 {
		detail["bytes"] = s.Bytes
	}
	if s.BytesSaved > 0 {
		detail["bytes_saved"] = s.BytesSaved
	}
	if s.Last != "" {
		detail["last"] = s.Last
	}
	if total := s.Total(); total > 0 {
		detail["total"] = total
		detail["hit_rate"] = s.Rate()
	}
	return detail
}

// notCountingNote is the note every counter-backed probe adds when the gate is
// closed.
//
// It exists because the alternative is indistinguishable from a middleware that
// is installed and never triggered, and those two want opposite next actions.
const notCountingNote = "Counted diagnostics are off for this process, so the numbers above are " +
	"not a report of an idle middleware — nothing was measured. Installing the dashboard or an " +
	"in-process MCP endpoint enables counting, as does calling diag.EnableCounters()."

// countedReport is the shared shape for a counter-backed probe.
//
// installed is what distinguishes "never wired up" from "wired up and quiet":
// each constructor sets its flag, so a probe can report StatusOff for a
// middleware the application never installed rather than reporting zeroes that
// read as a broken one.
func countedReport(installed bool, offReason, summaryVerb, hitLabel, missLabel string, c *diag.Counter) diag.Report {
	if !installed {
		return diag.Off(offReason)
	}

	snap := c.Snapshot()
	detail := counterDetail(snap, hitLabel, missLabel)

	report := diag.OK(fmt.Sprintf("installed; %d %s, %d %s",
		snap.Hits, summaryVerb, snap.Misses, missLabel), detail)
	if !snap.Counting {
		return report.WithNotes(notCountingNote)
	}
	return report
}
