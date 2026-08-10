package observability

import (
	"strings"
	"time"
)

// Source identifies the framework subsystem a signal came from.
//
// The event bus is the first producer, but the model is deliberately
// subsystem-agnostic: the router, cache, scheduler, database layer,
// WebSocket engine and OAuth2 middleware all describe their work with the
// same shape, so the dashboard needs one renderer rather than one per
// subsystem.
type Source string

// Known signal sources. Any string is valid; these are the ones Breeze
// itself publishes.
const (
	SourceEvents    Source = "events"
	SourceRouter    Source = "router"
	SourceHTTP      Source = "http"
	SourceCache     Source = "cache"
	SourceDatabase  Source = "database"
	SourceScheduler Source = "scheduler"
	SourceWebSocket Source = "websocket"
	SourceOAuth2    Source = "oauth2"
	SourcePlugin    Source = "plugin"
	SourceWorkflow  Source = "workflow"
)

// Kind describes the shape of a signal within its source.
type Kind string

// Known signal kinds.
const (
	// KindDispatch is one complete unit of work: an event dispatch, an
	// HTTP request, a scheduled job run.
	KindDispatch Kind = "dispatch"

	// KindListener is a child unit that completed after its parent was
	// already recorded, which is how asynchronous work is reported.
	KindListener Kind = "listener"

	// KindWorkflow is one workflow execution. Its steps are carried as
	// [Span] values, in the order they ran.
	KindWorkflow Kind = "workflow"

	// KindStep is a single workflow step recorded on its own, used when a
	// step is durable and outlives the execution that started it.
	KindStep Kind = "step"
)

// Signal is one observed occurrence anywhere in the framework.
//
// It is a flat, immutable, JSON-friendly value: everything the dashboard
// needs is already resolved, so rendering never reaches back into the
// subsystem that produced it. Errors are stored as strings rather than
// error values because a Signal outlives the goroutine that created it
// and may be serialised long afterwards.
type Signal struct {
	// ID is the observability layer's own monotonic identifier.
	ID uint64 `json:"id"`

	// SourceID is the producing subsystem's identifier for this unit of
	// work, such as the event bus's dispatch id. It is what child signals
	// point at through ParentID.
	SourceID uint64 `json:"source_id"`

	// ParentID links a child signal to its parent's SourceID. Zero for
	// top-level signals.
	ParentID uint64 `json:"parent_id,omitempty"`

	// Source is the producing subsystem.
	Source Source `json:"source"`

	// Kind is the shape of this signal.
	Kind Kind `json:"kind"`

	// Name identifies what happened: an event name, a route pattern, a
	// job name.
	Name string `json:"name"`

	// Time is when the unit of work started.
	Time time.Time `json:"time"`

	// Duration is how long it took.
	Duration time.Duration `json:"duration"`

	// DurationMS is Duration in milliseconds, carried explicitly so the
	// browser does not have to convert nanoseconds.
	DurationMS float64 `json:"duration_ms"`

	// Err is the failure message, or empty on success.
	Err string `json:"error,omitempty"`

	// Failed reports whether the unit of work failed.
	Failed bool `json:"failed"`

	// Cancelled reports whether it stopped early by design rather than by
	// failure.
	Cancelled bool `json:"cancelled,omitempty"`

	// Async reports whether the work was scheduled rather than awaited.
	Async bool `json:"async,omitempty"`

	// CorrelationID ties this signal to a wider logical operation.
	CorrelationID string `json:"correlation_id,omitempty"`

	// RequestID ties this signal to an inbound request.
	RequestID string `json:"request_id,omitempty"`

	// Children is the number of child units considered.
	Children int `json:"children"`

	// Executed is the number of child units that actually ran.
	Executed int `json:"executed"`

	// Size is the in-memory width of the payload in bytes, when known.
	Size int `json:"size,omitempty"`

	// Attrs carries subsystem-specific detail. Values are masked before
	// they are stored, so a Signal never holds a secret.
	Attrs map[string]string `json:"attrs,omitempty"`

	// Spans are the child units of work, in execution order.
	Spans []Span `json:"spans,omitempty"`
}

// Span is one child unit of work inside a Signal — an event listener, a
// middleware frame, a query within a request.
type Span struct {
	// Name identifies the child unit.
	Name string `json:"name"`

	// Duration is how long it took.
	Duration time.Duration `json:"duration"`

	// DurationMS is Duration in milliseconds, for the browser.
	DurationMS float64 `json:"duration_ms"`

	// Err is the failure message, or empty on success.
	Err string `json:"error,omitempty"`

	// Failed reports whether the child unit failed.
	Failed bool `json:"failed,omitempty"`

	// Panicked reports whether it panicked.
	Panicked bool `json:"panicked,omitempty"`

	// Skipped reports that it was considered but did not run.
	Skipped bool `json:"skipped,omitempty"`

	// Stopped reports that it halted propagation deliberately.
	Stopped bool `json:"stopped,omitempty"`

	// Priority is the child unit's ordering weight, when it has one.
	Priority int `json:"priority"`

	// Phase is the lifecycle band the child ran in.
	Phase string `json:"phase,omitempty"`

	// Index is the child's position in the execution order, or -1 when it
	// has no deterministic position.
	Index int `json:"index"`
}

// sensitiveTokens are substrings that mark an attribute as secret.
//
// Matching is done on a lowercased key, so "Authorization",
// "X-Auth-Token" and "user_password" are all caught. The list is
// deliberately broad: a false positive costs a masked debugging value,
// while a false negative writes a credential into a ring buffer that the
// dashboard will happily render.
var sensitiveTokens = []string{
	"password", "passwd", "secret", "token", "authorization", "auth",
	"cookie", "session", "credential", "apikey", "api_key", "private",
	"signature", "csrf", "bearer", "refresh", "access_key", "client_secret",
	"salt", "hash", "otp", "pin",
}

// maskedValue replaces every secret, regardless of its original length,
// so the mask itself cannot be used to infer anything.
const maskedValue = "[REDACTED]"

// IsSensitive reports whether an attribute key names a secret.
func IsSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, t := range sensitiveTokens {
		if strings.Contains(k, t) {
			return true
		}
	}
	return false
}

// MaskAttr returns the value to store for the given key, redacting it
// when the key names a secret.
func MaskAttr(key, value string) string {
	if IsSensitive(key) {
		return maskedValue
	}
	return value
}

// MaskAttrs returns a copy of attrs with every sensitive value redacted.
// It returns nil for an empty input so that an unused Attrs map never
// costs an allocation.
func MaskAttrs(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		out[k] = MaskAttr(k, v)
	}
	return out
}
