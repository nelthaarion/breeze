package aggregator

import (
	"strings"
	"time"

	"github.com/nelthaarion/breeze/events"
)

// Defaults from §8.1 and §10.
const (
	DefaultBasePath         = "/fleet"
	DefaultMaxTraces        = 2000
	DefaultMaxSpansPerTrace = 512
	DefaultTraceTTL         = 5 * time.Minute
	DefaultServiceTTL       = 15 * time.Second
	DefaultMaxViolations    = 1000

	// DefaultBlastRadiusErrorRate is the error rate at which a service is
	// treated as degraded for blast-radius purposes (§9B.2). 10% is high
	// enough that ordinary 4xx-ish noise and a few timeouts do not trip it,
	// and low enough to fire well before a service is fully down.
	DefaultBlastRadiusErrorRate = 0.10

	// DefaultBlastRadiusWindow is the rolling window error rate is measured
	// over. One minute is short enough to notice an incident starting and long
	// enough that a single unlucky second cannot trigger a banner.
	DefaultBlastRadiusWindow = time.Minute
)

// Config configures an Aggregator (§8.1).
type Config struct {
	// BasePath is where the aggregator's routes are mounted. Defaults to
	// "/fleet", matching the URL the dashboard's proxy expects.
	BasePath string

	// Username and Password guard the read side (§11.2). Both must be
	// non-empty for auth to be enforced — the same rule, and the same
	// wording, as the dashboard's own basic auth, so an operator who knows
	// one knows the other. Empty means no auth and a startup warning.
	Username string
	Password string

	// IngestToken guards the write side (§11.1), checked in constant time.
	//
	// Deliberately separate from the credentials above: services need to
	// push, humans need to read, and using one secret for both would put the
	// password a person logs in with into the config of every service.
	IngestToken string

	// ServiceToken authenticates the aggregator when it fetches a service's
	// own logs for trace-correlated stitching (§9C.2). It must match that
	// service's dashboard.Config.ServiceToken.
	//
	// Separate from IngestToken because the direction of trust is reversed:
	// IngestToken proves a service may *write* to the aggregator, while this
	// proves the aggregator may *read* a service's logs. Reusing one secret
	// for both would mean any service able to push spans could also read
	// every other service's logs. Empty disables fan-out entirely rather
	// than attempting it unauthenticated — logs are the most sensitive data
	// this feature moves, so the default is to not move them.
	ServiceToken string

	// TransportsEnabled lists ingestion transports the process exposes. HTTP
	// is always available as the polyglot correctness baseline; events is the
	// framework default. Unknown names are ignored by the base package so an
	// optional transport sub-package can own its own listener.
	TransportsEnabled []string

	// EventsBus is used by the zero-copy, same-process events transport. Nil
	// selects events.Default. Broker-backed event transports remain isolated
	// in their own packages and feed the same topic schema.
	EventsBus *events.Bus

	// MaxTraces bounds how many traces are retained. Oldest-first eviction.
	MaxTraces int

	// MaxSpansPerTrace bounds one trace. Guards against a retry loop or a
	// runaway fan-out turning a single trace into an unbounded allocation.
	MaxSpansPerTrace int

	// TraceTTL is how long a trace is kept after its last span. Doubles as
	// the definition of "complete": nothing waits for an end-of-trace signal,
	// because no such signal can exist when any service may crash mid-trace.
	TraceTTL time.Duration

	// ServiceTTL is how long a service may go without a heartbeat before it
	// is marked down. Must be comfortably more than a Tracer's heartbeat
	// interval or healthy services flap.
	ServiceTTL time.Duration

	// ContractValidation enables §9A's live schema checking.
	ContractValidation bool

	// MaxViolations bounds the violation ring buffer.
	MaxViolations int

	// ViolationDedupeWindow groups identical violations, so one bad deploy
	// produces a row with a count rather than thousands of near-identical
	// rows.
	ViolationDedupeWindow time.Duration

	// BlastRadiusErrorRateThreshold and BlastRadiusWindow control §9B.2.
	BlastRadiusErrorRateThreshold float64
	BlastRadiusWindow             time.Duration

	// Logger receives the aggregator's diagnostics, in the same
	// (level, message, source) shape as the dashboard collector's PushLog so
	// a caller can pass it directly. Nil discards.
	Logger func(level, message, source string)
}

// DefaultConfig returns a Config with every bound populated.
//
// Every field that bounds memory has a non-zero default here, because a
// zero-valued bound is unbounded, and an aggregator that quietly ignores its
// limits is a memory leak waiting for the first traffic spike.
func DefaultConfig() Config {
	return Config{
		BasePath:                      DefaultBasePath,
		MaxTraces:                     DefaultMaxTraces,
		MaxSpansPerTrace:              DefaultMaxSpansPerTrace,
		TraceTTL:                      DefaultTraceTTL,
		ServiceTTL:                    DefaultServiceTTL,
		TransportsEnabled:             []string{"events", "http"},
		ContractValidation:            true,
		MaxViolations:                 DefaultMaxViolations,
		ViolationDedupeWindow:         time.Minute,
		BlastRadiusErrorRateThreshold: DefaultBlastRadiusErrorRate,
		BlastRadiusWindow:             DefaultBlastRadiusWindow,
	}
}

// withDefaults fills unset fields, so a partially-specified Config is safe.
//
// The pattern is deliberate: an operator setting only MaxTraces should not
// thereby set MaxSpansPerTrace to zero and lose every span.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.BasePath == "" {
		c.BasePath = d.BasePath
	}
	if c.MaxTraces <= 0 {
		c.MaxTraces = d.MaxTraces
	}
	if c.MaxSpansPerTrace <= 0 {
		c.MaxSpansPerTrace = d.MaxSpansPerTrace
	}
	if c.TraceTTL <= 0 {
		c.TraceTTL = d.TraceTTL
	}
	if c.ServiceTTL <= 0 {
		c.ServiceTTL = d.ServiceTTL
	}
	if len(c.TransportsEnabled) == 0 {
		c.TransportsEnabled = append([]string(nil), d.TransportsEnabled...)
	}
	if c.MaxViolations <= 0 {
		c.MaxViolations = d.MaxViolations
	}
	if c.ViolationDedupeWindow <= 0 {
		c.ViolationDedupeWindow = d.ViolationDedupeWindow
	}
	if c.BlastRadiusErrorRateThreshold <= 0 {
		c.BlastRadiusErrorRateThreshold = d.BlastRadiusErrorRateThreshold
	}
	if c.BlastRadiusWindow <= 0 {
		c.BlastRadiusWindow = d.BlastRadiusWindow
	}
	return c
}

// AuthEnabled reports whether read-side basic auth is enforced.
//
// Both fields must be non-empty, mirroring the dashboard exactly. A username with
// no password is a misconfiguration that would otherwise silently accept any
// password, which is worse than no auth at all because it looks protected.
func (c Config) AuthEnabled() bool { return c.Username != "" && c.Password != "" }

func (c Config) transportEnabled(name string) bool {
	for _, candidate := range c.TransportsEnabled {
		if strings.EqualFold(strings.TrimSpace(candidate), name) {
			return true
		}
	}
	return false
}
