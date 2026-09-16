package events

// DatabaseQuery is emitted by database integrations after a query completes.
// It keeps database query telemetry on the framework event bus so observers,
// the dashboard and application listeners all see the same typed event.
type DatabaseQuery struct {
	ID         string
	Time       int64 // Unix nanoseconds.
	SQL        string
	DurationUS int64
	Rows       int64
	File       string
	Line       int
	Slow       bool
	Error      string
}
