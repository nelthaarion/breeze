package events

import (
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
)

// Handler is the function signature every listener implements.
//
// The Context is shared across the dispatch; the event is delivered by
// value, so a listener cannot mutate what later listeners observe. Use
// [Context.Set] to pass data down the chain instead.
//
// Returning nil continues the dispatch. Returning [Stop] halts it without
// signalling failure. Returning any other error halts it and surfaces
// from Emit, unless [Config.ContinueOnError] is set.
type Handler[T any] func(ctx *Context, event T) error

// Filter decides whether a listener should run for a given event.
// Returning false skips the listener entirely — its handler is never
// entered and it is not counted as an invocation.
type Filter[T any] func(event T) bool

// phase orders listeners into the three lifecycle bands. Before-hooks all
// run ahead of normal listeners, which all run ahead of after-hooks,
// regardless of the priorities within each band.
type phase int8

const (
	phaseBefore phase = iota - 1
	phaseNormal
	phaseAfter
)

// String implements fmt.Stringer.
func (p phase) String() string {
	switch p {
	case phaseBefore:
		return "before"
	case phaseAfter:
		return "after"
	default:
		return "normal"
	}
}

// Priority constants for the common cases. Any int is accepted; these
// exist so framework subsystems agree on where they sit relative to
// application code.
const (
	// PriorityHighest runs before everything else. Reserved for
	// validation and security checks that must gate later work.
	PriorityHighest = 1000
	// PriorityHigh runs early. Suited to normalisation and enrichment.
	PriorityHigh = 100
	// PriorityNormal is the default for listeners that do not set one.
	PriorityNormal = 0
	// PriorityLow runs late. Suited to persistence.
	PriorityLow = -100
	// PriorityLowest runs last. Suited to auditing and metrics.
	PriorityLowest = -1000
)

// listener is one registered handler.
//
// # Immutability
//
// A listener is immutable once it is published in a snapshot. The chained
// modifiers ([Subscription.Priority], [Subscription.Where]) do not write
// to a live listener; they clone it, modify the clone, and publish a new
// snapshot. Dispatch can therefore read every field without
// synchronisation.
//
// The two exceptions are the atomic pointers `fired` and `calls`, which
// are shared by every clone precisely so that mutation survives a rebuild:
// a once-listener that has already run must stay spent even if its
// priority is changed afterwards.
type listener[T any] struct {
	fn     Handler[T]
	filter Filter[T]

	// fired guards a once-listener so that exactly one goroutine runs it,
	// even if two dispatches race. Nil for ordinary listeners.
	fired *atomic.Bool

	// calls counts invocations for the inspector. Shared across clones.
	calls *atomic.Uint64

	name     string
	id       uint64
	priority int
	phase    phase
	once     bool
}

// invoke runs the listener's filter and handler.
//
// It returns skipped=true when the filter rejected the event or when a
// once-listener had already fired, so the caller can avoid counting a
// non-execution as an execution.
func (l *listener[T]) invoke(ctx *Context, event T) (err error, skipped bool) {
	if l.filter != nil && !l.filter(event) {
		return nil, true
	}
	// CompareAndSwap rather than a load-then-store: under EmitAsync two
	// goroutines can reach the same once-listener concurrently, and only
	// one of them may proceed.
	if l.once && !l.fired.CompareAndSwap(false, true) {
		return nil, true
	}
	l.calls.Add(1)
	return l.fn(ctx, event), false
}

// clone returns a shallow copy for modification. The atomic pointers are
// deliberately shared with the original rather than copied.
func (l *listener[T]) clone() *listener[T] {
	c := *l
	return &c
}

// compareListeners defines the total execution order:
//
//  1. phase ascending — before, then normal, then after;
//  2. priority descending — higher priority runs first;
//  3. id ascending — registration order breaks ties, so two listeners
//     with equal priority always run in the order they were added.
//
// Because id is unique the ordering is total, which means an unstable
// sort is sufficient and the result is deterministic.
//
// It compares and returns -1/0/1 rather than subtracting: priorities are
// documented as "any int", and b.priority - a.priority overflows for extreme
// values (math.MaxInt against math.MinInt) and returns the wrong sign. The
// comparisons are spelled out instead of calling cmp.Compare because this is
// the sort's inner loop and the three generic calls were measurably slower.
func compareListeners[T any](a, b *listener[T]) int {
	if a.phase != b.phase {
		if a.phase < b.phase {
			return -1
		}
		return 1
	}
	if a.priority != b.priority {
		if a.priority > b.priority { // higher priority runs first
			return -1
		}
		return 1
	}
	switch {
	case a.id < b.id:
		return -1
	case a.id > b.id:
		return 1
	default:
		return 0
	}
}

// sortListeners orders a snapshot in place.
//
// This runs only on registration and modification. A dispatch consumes an
// already-sorted snapshot and never sorts.
func sortListeners[T any](ls []*listener[T]) {
	slices.SortFunc(ls, compareListeners[T])
}

// handlerName derives a readable name from a function value using its
// program counter.
//
// This is the one place the package inspects a func with reflection, and
// it happens once per registration — never during a dispatch. The full
// symbol (github.com/org/app/mail.SendWelcome) is trimmed to the last
// package-qualified segment (mail.SendWelcome) to keep inspector output
// readable, and the compiler's closure suffixes are preserved because
// they are often the only way to tell two closures apart.
func handlerName(fn any) string {
	if fn == nil {
		return "<nil>"
	}
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func || v.IsNil() {
		return "<nil>"
	}
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return "<anonymous>"
	}
	full := f.Name()
	if i := strings.LastIndexByte(full, '/'); i >= 0 {
		full = full[i+1:]
	}
	return full
}
