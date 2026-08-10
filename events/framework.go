package events

import "time"

// This file declares the events Breeze subsystems publish. They are
// ordinary structs, so an application listens to them exactly as it would
// its own events:
//
//	events.On(events.RequestFinished{}, func(ctx *events.Context, e events.RequestFinished) error {
//		metrics.Observe(e.Route, e.Duration, e.Status)
//		return nil
//	})
//
// Every type is registered with a dotted name in init so the dashboard
// and logs show "http.request.finished" rather than a Go type path.
//
// Compatibility: fields may be added to these structs in a minor release.
// Construct them with field names, not positionally, and they will keep
// compiling.

// --- Application lifecycle ---

// ApplicationStarting is emitted before the server begins listening.
// Listeners may still mutate configuration at this point.
type ApplicationStarting struct {
	// Address is the address the server is about to bind.
	Address string
	// Time is when startup began.
	Time time.Time
}

// ApplicationStarted is emitted once the server is accepting connections.
type ApplicationStarted struct {
	Address string
	// StartupDuration is how long startup took.
	StartupDuration time.Duration
	Time            time.Time
}

// ApplicationStopping is emitted at the start of a graceful shutdown,
// before connections are drained. It is the hook for flushing buffers and
// closing external resources.
type ApplicationStopping struct {
	// Reason describes what triggered the shutdown, e.g. "signal:
	// SIGTERM".
	Reason string
	Time   time.Time
}

// ApplicationStopped is emitted once shutdown has completed.
type ApplicationStopped struct {
	// Uptime is how long the application ran.
	Uptime time.Duration
	Time   time.Time
}

// --- Router ---

// RouteRegistered is emitted when a route is added to the router.
type RouteRegistered struct {
	Method  string
	Path    string
	Handler string
}

// RouteRemoved is emitted when a route is removed.
type RouteRemoved struct {
	Method string
	Path   string
}

// MiddlewareAdded is emitted when middleware is attached to the router or
// to a group.
type MiddlewareAdded struct {
	Name string
	// Scope is the path prefix the middleware applies to; empty means
	// global.
	Scope string
}

// --- HTTP ---

// RequestStarted is emitted when a request begins processing.
//
// It is on the hot path of every request, so listeners should be cheap;
// prefer [EmitAsync] for anything that performs I/O.
type RequestStarted struct {
	RequestID string
	Method    string
	Path      string
	RemoteIP  string
	UserAgent string
	Time      time.Time
}

// RequestFinished is emitted once a response has been written.
type RequestFinished struct {
	RequestID string
	Method    string
	Path      string
	// Route is the matched route pattern, e.g. "/users/:id". It is the
	// field to group metrics by; Path contains the concrete values.
	Route    string
	Status   int
	Duration time.Duration
	// BytesWritten is the size of the response body.
	BytesWritten int64
}

// RequestPanicked is emitted when a handler panics and recovery catches
// it.
type RequestPanicked struct {
	RequestID string
	Method    string
	Path      string
	// Value is the value passed to panic().
	Value any
	Stack []byte
}

// --- Authentication ---

// UserAuthenticated is emitted on a successful authentication.
type UserAuthenticated struct {
	UserID    string
	Method    string // "jwt", "session", "oauth2", ...
	RequestID string
	Time      time.Time
}

// AuthenticationFailed is emitted on a rejected authentication attempt.
type AuthenticationFailed struct {
	// Reason is why authentication failed. Do not place credentials in
	// it: this event reaches logs and the dashboard.
	Reason    string
	Method    string
	RemoteIP  string
	RequestID string
	Time      time.Time
}

// UserLoggedOut is emitted when a session ends.
type UserLoggedOut struct {
	UserID    string
	SessionID string
	Time      time.Time
}

// --- OAuth2 ---

// OAuthStarted is emitted when an authorization flow begins.
type OAuthStarted struct {
	Provider string
	State    string
	Scopes   []string
}

// OAuthSucceeded is emitted when the provider callback is validated and
// tokens are issued.
type OAuthSucceeded struct {
	Provider string
	UserID   string
	Email    string
}

// OAuthFailed is emitted when a flow fails.
type OAuthFailed struct {
	Provider string
	Reason   string
	// Err is the underlying error, when there is one.
	Err error
}

// TokenRefreshed is emitted when an access token is renewed.
type TokenRefreshed struct {
	Provider string
	UserID   string
	// ExpiresAt is when the new token expires.
	ExpiresAt time.Time
}

// --- Plugins ---

// PluginInstalled is emitted when a plugin is registered.
type PluginInstalled struct {
	Name    string
	Version string
}

// PluginEnabled is emitted when a plugin is activated.
type PluginEnabled struct {
	Name string
}

// PluginDisabled is emitted when a plugin is deactivated.
type PluginDisabled struct {
	Name   string
	Reason string
}

// PluginReloaded is emitted when a plugin is reloaded in place.
type PluginReloaded struct {
	Name       string
	OldVersion string
	NewVersion string
}

// --- WebSocket ---

// ClientConnected is emitted when a WebSocket client completes the
// handshake.
type ClientConnected struct {
	ConnectionID string
	RemoteIP     string
	Path         string
	Time         time.Time
}

// ClientDisconnected is emitted when a WebSocket client disconnects.
type ClientDisconnected struct {
	ConnectionID string
	// Code is the WebSocket close code.
	Code     int
	Reason   string
	Duration time.Duration
}

// MessageReceived is emitted for an inbound WebSocket frame.
type MessageReceived struct {
	ConnectionID string
	// Type is the frame type: 1 for text, 2 for binary.
	Type int
	Size int
}

// MessageSent is emitted for an outbound WebSocket frame.
type MessageSent struct {
	ConnectionID string
	Type         int
	Size         int
}

// --- Workflow ---
//
// Workflow events carry identifiers and primitives only, never the
// workflow's payload or its step handlers, so that this package stays
// free of any dependency on the workflow engine.

// WorkflowStarted is emitted when a workflow execution begins.
type WorkflowStarted struct {
	Workflow    string
	ExecutionID string
	// Trigger names the event type that started the execution, or is
	// empty when it was started directly.
	Trigger string
	Steps   int
	// StepNames lists every step in declaration order, including the
	// ones that have not run yet. A consumer watching an execution
	// unfold needs the whole plan up front: knowing only the count
	// would let it show progress but never say what comes next.
	StepNames []string
	Time      time.Time
}


// WorkflowStepStarted is emitted before a step's handler runs.
type WorkflowStepStarted struct {
	Workflow    string
	ExecutionID string
	Step        string
	// Attempt is 1 for the first try and increments on every retry.
	Attempt int
}

// WorkflowStepCompleted is emitted when a step's handler returns nil.
type WorkflowStepCompleted struct {
	Workflow    string
	ExecutionID string
	Step        string
	Attempt     int
	Duration    time.Duration
}

// WorkflowStepFailed is emitted when a step's handler returns an error,
// panics, or times out. It is emitted for every failed attempt, including
// those that will be retried.
type WorkflowStepFailed struct {
	Workflow    string
	ExecutionID string
	Step        string
	Attempt     int
	Duration    time.Duration
	Err         string
	// Retryable reports whether another attempt is scheduled.
	Retryable bool
}

// WorkflowRetrying is emitted when a failed step is scheduled for another
// attempt, after the backoff delay has been computed.
type WorkflowRetrying struct {
	Workflow    string
	ExecutionID string
	Step        string
	// Attempt is the attempt number that is about to run.
	Attempt int
	Delay   time.Duration
}

// WorkflowTimedOut is emitted when a workflow or one of its steps exceeds
// its deadline. Step is empty when the whole execution timed out.
type WorkflowTimedOut struct {
	Workflow    string
	ExecutionID string
	Step        string
	Timeout     time.Duration
}

// WorkflowCancelled is emitted when an execution stops because its
// context was cancelled.
type WorkflowCancelled struct {
	Workflow    string
	ExecutionID string
	Step        string
	Reason      string
}

// WorkflowCompleted is emitted when every required step has succeeded.
type WorkflowCompleted struct {
	Workflow    string
	ExecutionID string
	Steps       int
	Duration    time.Duration
}

// WorkflowFailed is emitted when an execution ends without completing.
type WorkflowFailed struct {
	Workflow    string
	ExecutionID string
	// Step names the step that caused the failure, when there was one.
	Step     string
	Duration time.Duration
	Err      string
}

// WorkflowCompensationStarted is emitted when a failure triggers rollback
// of the steps that had already succeeded.
type WorkflowCompensationStarted struct {
	Workflow    string
	ExecutionID string
	// Steps is the number of completed steps that will be compensated.
	Steps int
	// Cause is the error that triggered compensation.
	Cause string
}

// WorkflowCompensationCompleted is emitted when every compensation
// handler has run successfully.
type WorkflowCompensationCompleted struct {
	Workflow    string
	ExecutionID string
	Steps       int
	Duration    time.Duration
}

// WorkflowCompensationFailed is emitted when a compensation handler
// itself fails, which leaves the execution in a state that needs
// operator attention.
type WorkflowCompensationFailed struct {
	Workflow    string
	ExecutionID string
	Step        string
	Err         string
}

// --- Scheduler ---

// JobStarted is emitted when a scheduled job begins.
type JobStarted struct {
	JobID string
	Name  string
	Time  time.Time
}

// JobFinished is emitted when a job completes successfully.
type JobFinished struct {
	JobID    string
	Name     string
	Duration time.Duration
}

// JobFailed is emitted when a job returns an error or panics.
type JobFailed struct {
	JobID    string
	Name     string
	Err      error
	Duration time.Duration
	// Attempt is the 1-based retry attempt that failed.
	Attempt int
}

// --- Configuration ---

// ConfigReloaded is emitted when configuration is re-read at runtime.
type ConfigReloaded struct {
	Source string
	// Changed lists the keys whose values differ from the previous load.
	Changed []string
	Time    time.Time
}

// init registers the dotted display names for the framework events.
//
// Naming them here means the dashboard shows stable identifiers from the
// first dispatch, without every subsystem having to remember to name its
// own events.
func init() {
	SetName[ApplicationStarting]("app.starting")
	SetName[ApplicationStarted]("app.started")
	SetName[ApplicationStopping]("app.stopping")
	SetName[ApplicationStopped]("app.stopped")

	SetName[RouteRegistered]("router.route.registered")
	SetName[RouteRemoved]("router.route.removed")
	SetName[MiddlewareAdded]("router.middleware.added")

	SetName[RequestStarted]("http.request.started")
	SetName[RequestFinished]("http.request.finished")
	SetName[RequestPanicked]("http.request.panicked")

	SetName[UserAuthenticated]("auth.user.authenticated")
	SetName[AuthenticationFailed]("auth.failed")
	SetName[UserLoggedOut]("auth.user.logged_out")

	SetName[OAuthStarted]("oauth.started")
	SetName[OAuthSucceeded]("oauth.succeeded")
	SetName[OAuthFailed]("oauth.failed")
	SetName[TokenRefreshed]("oauth.token.refreshed")

	SetName[PluginInstalled]("plugin.installed")
	SetName[PluginEnabled]("plugin.enabled")
	SetName[PluginDisabled]("plugin.disabled")
	SetName[PluginReloaded]("plugin.reloaded")

	SetName[ClientConnected]("ws.client.connected")
	SetName[ClientDisconnected]("ws.client.disconnected")
	SetName[MessageReceived]("ws.message.received")
	SetName[MessageSent]("ws.message.sent")

	SetName[WorkflowStarted]("workflow.started")
	SetName[WorkflowStepStarted]("workflow.step.started")
	SetName[WorkflowStepCompleted]("workflow.step.completed")
	SetName[WorkflowStepFailed]("workflow.step.failed")
	SetName[WorkflowRetrying]("workflow.retrying")
	SetName[WorkflowTimedOut]("workflow.timed_out")
	SetName[WorkflowCancelled]("workflow.cancelled")
	SetName[WorkflowCompleted]("workflow.completed")
	SetName[WorkflowFailed]("workflow.failed")
	SetName[WorkflowCompensationStarted]("workflow.compensation.started")
	SetName[WorkflowCompensationCompleted]("workflow.compensation.completed")
	SetName[WorkflowCompensationFailed]("workflow.compensation.failed")

	SetName[JobStarted]("scheduler.job.started")
	SetName[JobFinished]("scheduler.job.finished")
	SetName[JobFailed]("scheduler.job.failed")

	SetName[ConfigReloaded]("config.reloaded")
}
