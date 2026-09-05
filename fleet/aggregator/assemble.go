package aggregator

// Trace assembly: turning a bag of spans reported independently by several
// services into one tree, and answering the two questions someone actually opens
// a trace to ask — what broke, and what did it take down with it (§8.1.3, §8.1.4,
// §9B.1).
//
// # Assembly must never lose data
//
// Spans arrive out of order, late, and incomplete. A service may crash before
// exporting; a sampling mismatch may mean a parent was never recorded. Every one
// of those produces a span whose parent is absent. Dropping such spans would hide
// exactly the failures worth seeing — the crashed service's own children — so
// they are re-rooted as orphans and marked, never discarded.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// Trace is one assembled request journey, ready to render.
type Trace struct {
	TraceID string `json:"trace_id"`

	// Roots are the top-level spans. Usually one; more than one means either
	// a genuine multi-entry trace or, more often, orphans whose parents were
	// never reported.
	Roots []*SpanNode `json:"roots"`

	Services     []string `json:"services"`
	SpanCount    int      `json:"span_count"`
	DurationMs   float64  `json:"duration_ms"`
	StartNanoUTC int64    `json:"start_ns"`
	HasError     bool     `json:"has_error"`
	Status       int      `json:"status"`

	// SkewFlagged marks a trace where at least one child appeared to start
	// before its parent — see clampSkew.
	SkewFlagged bool `json:"skew_flagged"`

	// OrphanCount is how many spans could not be linked to their parent.
	// Surfaced rather than hidden: a non-zero value tells the viewer the tree
	// is incomplete, which changes how they read the gaps in it.
	OrphanCount int `json:"orphan_count"`

	// SpansDropped is how many spans this trace lost to MaxSpansPerTrace.
	SpansDropped int `json:"spans_dropped,omitempty"`

	// RootCauseSpanID is the failing span the failure originated in (§9B.1) —
	// the deepest one, not the earliest-starting; see markRootCause. Empty
	// when nothing failed.
	RootCauseSpanID string `json:"root_cause_span_id,omitempty"`

	// Summary is a plain-fact, template-built sentence describing the failure
	// (§9B.1). Deterministic string formatting — no model, no inference.
	Summary string `json:"summary,omitempty"`

	FirstSeenUnixNano int64 `json:"first_seen_ns,omitempty"`
}

// SpanNode is a span plus its position in the tree.
type SpanNode struct {
	fleet.Span

	Children []*SpanNode `json:"children,omitempty"`

	// Orphan marks a span whose parent was never reported, so the UI can show
	// a "missing parent" marker instead of implying this span began the trace.
	Orphan bool `json:"orphan,omitempty"`

	// Skewed marks a span whose start time preceded its parent's, which is
	// physically impossible and therefore a clock-skew artifact (§8.1.4).
	Skewed bool `json:"skewed,omitempty"`

	// RootCause and DerivedError implement §9B.1: exactly one span in a failing
	// trace is the cause, and the rest of the red spans are its consequences.
	RootCause    bool `json:"root_cause,omitempty"`
	DerivedError bool `json:"derived_error,omitempty"`
}

// Assemble builds a Trace from spans belonging to one trace id.
//
// One function doing sort, link, skew-check, and root-cause marking because they
// all need the same traversal in the same order, and splitting them into four
// passes over the tree would be slower for no clarity gain.
func Assemble(traceID string, spans []fleet.Span) Trace {
	tr := Trace{TraceID: traceID, SpanCount: len(spans)}
	if len(spans) == 0 {
		return tr
	}

	// Causal order. Ties broken by span id so assembly is deterministic:
	// without a tiebreak, two spans sharing a start timestamp (common at
	// millisecond resolution on a fast local call) could order differently
	// between two requests for the same trace, and root-cause marking — which
	// breaks ties between independent failing branches by start time — would
	// flip between reloads.

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].StartNanoUTC != spans[j].StartNanoUTC {
			return spans[i].StartNanoUTC < spans[j].StartNanoUTC
		}
		return spans[i].SpanID < spans[j].SpanID
	})

	nodes := make(map[string]*SpanNode, len(spans))
	order := make([]*SpanNode, 0, len(spans))
	for i := range spans {
		n := &SpanNode{Span: spans[i]}
		// A duplicate span id means a service re-exported after a failed
		// flush — the Tracer requeues on error, and a response that was
		// lost in transit produces exactly this. Keeping the first and
		// ignoring the retry is what makes export retries idempotent
		// here; without it a retried batch would duplicate every node in
		// the tree.
		if _, dup := nodes[n.SpanID]; dup {
			tr.SpanCount--
			continue
		}
		nodes[n.SpanID] = n
		order = append(order, n)
	}

	// Resolve each span's parent pointer, then break any cycles *before*
	// building the child lists. Order matters: a cycle in the child lists
	// makes the tree walks below loop forever, and these spans come from the
	// network — a buggy or hostile service can report whatever parentage it
	// likes, so an unkillable request handler must not be reachable that way.
	parentOf := make([]*SpanNode, len(order))
	for i, n := range order {
		if n.ParentSpanID == "" {
			continue
		}
		parent, ok := nodes[n.ParentSpanID]
		if !ok || parent == n {
			// Either the parent never reported (§8.1.3) or the span
			// names itself, which is a one-node cycle.
			continue
		}
		parentOf[i] = parent
	}
	tr.breakCycles(order, parentOf)

	for i, n := range order {
		if parentOf[i] == nil {
			// No parent in this trace. A span that never claimed one
			// legitimately starts the trace; one whose claimed parent is
			// absent is an orphan and says so.
			if n.ParentSpanID != "" && !n.Orphan {
				n.Orphan = true
				tr.OrphanCount++
			}
			tr.Roots = append(tr.Roots, n)
			continue
		}
		parentOf[i].Children = append(parentOf[i].Children, n)
	}

	tr.aggregateFacts(order)
	tr.clampSkew()
	tr.markRootCause(order)
	return tr
}

// breakCycles severs the minimum number of parent links needed to make the
// parentage acyclic, marking each severed span as an orphan.
//
// A cycle cannot happen in a correct fleet — parentage follows causality, and a
// request cannot cause itself. It can happen through a span-id collision, a
// service that reuses ids across requests, a replayed batch, or a service that
// simply reports whatever it likes. Since these spans arrive over the network,
// "cannot happen" is not a safety argument: the tree walks below would loop
// forever on a cycle, hanging the request handler that serves the trace and,
// with enough such requests, the aggregator itself. Breaking cycles at assembly
// time makes that unreachable by construction rather than by assumption.
//
// The classic iterative colouring walk: follow each unvisited span's parent
// chain, and if it re-enters the chain currently being followed, cut the link
// that closed the loop. Linear in the number of spans, no recursion.
func (tr *Trace) breakCycles(order []*SpanNode, parentOf []*SpanNode) {
	const (
		unvisited = 0
		onPath    = 1 // in the chain being followed right now
		settled   = 2 // known to reach a root without cycling
	)

	index := make(map[*SpanNode]int, len(order))
	for i, n := range order {
		index[n] = i
	}
	state := make([]uint8, len(order))
	var path []int

	for i := range order {
		if state[i] != unvisited {
			continue
		}
		path = path[:0]
		j := i
		for {
			if state[j] == onPath {
				// Closed a loop: cut the link that did it. The span
				// becomes a root and is marked, so the UI shows an
				// incomplete tree rather than silently omitting it.
				parentOf[j] = nil
				if !order[j].Orphan {
					order[j].Orphan = true
					tr.OrphanCount++
				}
				break
			}
			if state[j] == settled {
				break
			}
			state[j] = onPath
			path = append(path, j)

			parent := parentOf[j]
			if parent == nil {
				break
			}
			j = index[parent]
		}
		// Everything followed on this pass is now known to terminate.
		for _, k := range path {
			state[k] = settled
		}
	}
}

// aggregateFacts fills the summary-level fields from the span set.

func (tr *Trace) aggregateFacts(order []*SpanNode) {
	seen := make(map[string]struct{}, 4)
	var minStart, maxEnd int64
	for i, n := range order {
		if n.Service != "" {
			if _, dup := seen[n.Service]; !dup {
				seen[n.Service] = struct{}{}
				tr.Services = append(tr.Services, n.Service)
			}
		}
		end := n.StartNanoUTC + int64(n.DurationMs*float64(1e6))
		if i == 0 || n.StartNanoUTC < minStart {
			minStart = n.StartNanoUTC
		}
		if i == 0 || end > maxEnd {
			maxEnd = end
		}
		if n.Failed() {
			tr.HasError = true
		}
	}
	sort.Strings(tr.Services)
	tr.StartNanoUTC = minStart

	// Wall-clock span of the whole trace, which is *not* the sum of the
	// individual durations: hops overlap (a parent is still running while its
	// child runs) and parallel fan-out overlaps entirely. Summing would
	// report a 40ms request as 120ms across three services.
	tr.DurationMs = float64(maxEnd-minStart) / 1e6

	// The trace's status is the entry point's, because that is what the caller
	// actually received. A trace where an internal service 500'd but the
	// gateway degraded gracefully and returned 200 is a 200 to the user, and
	// showing it as a 500 would misrepresent the user-visible outcome — the
	// internal failure is still visible on its own span and in HasError.
	if len(tr.Roots) > 0 {
		tr.Status = tr.Roots[0].Status
	}
}

// clampSkew flags children that appear to start before their parent (§8.1.4).
//
// Services do not share a clock. A child recording a start time earlier than its
// parent's is impossible causally, so it means the two machines disagree about
// what time it is. The response is to flag, not to correct: estimating an offset
// and rewriting timestamps would produce a timeline that looks authoritative
// while being a guess. A visible warning that two clocks disagree is more useful
// than a silently adjusted picture, and full NTP-style correction is out of scope
// per the spec.
func (tr *Trace) clampSkew() {
	for _, root := range tr.Roots {
		walk(root, func(parent, child *SpanNode) {
			if parent == nil {
				return
			}
			if child.StartNanoUTC < parent.StartNanoUTC {
				child.Skewed = true
				tr.SkewFlagged = true
			}
		})
	}
}

// markRootCause implements §9B.1.
//
// In a cascading failure every service in the chain reports an error, so a naive
// view shows three red rows and leaves the reader to work out which one started
// it. Naming the true origin is the difference between "something is wrong
// somewhere" and "this is what broke".
//
// The origin is the *deepest* failure, not the earliest-starting one. Those are
// not the same thing, and picking the wrong one inverts the answer on the most
// common shape of incident there is. In a synchronous chain
// gateway → auth → orders, the gateway's span opens first and closes last: it is
// still waiting when orders fails underneath it, and only reports its own 500
// afterwards because its callee already did. Ranking by start time therefore
// always blames the outermost caller — the one span guaranteed to have started
// first — and buries the service that actually broke at the bottom of the tree,
// which is precisely the manual correlation this feature exists to remove.
//
// A failing span with no failing descendant is where the error originated: it
// had nothing underneath it to inherit the failure from. Ties between
// independent failing branches (a fan-out where two callees fail separately)
// fall back to the earliest start, which is meaningful *between siblings*
// because neither is waiting on the other.
func (tr *Trace) markRootCause(order []*SpanNode) {
	var cause *SpanNode
	for _, n := range order {
		if !n.Failed() || failedBelow(n) {
			continue
		}
		// order is sorted by start time with a span-id tiebreak, so the
		// first candidate found is the earliest deterministically.
		cause = n
		break
	}
	if cause == nil {
		// Every failing span has a failing descendant, which a finite
		// tree makes impossible; treat it as "no attributable origin"
		// rather than trusting an unreachable branch.
		return
	}

	cause.RootCause = true
	tr.RootCauseSpanID = cause.SpanID

	var derived []*SpanNode
	for _, n := range order {
		if n == cause || !n.Failed() {
			continue
		}
		n.DerivedError = true
		derived = append(derived, n)
	}
	tr.Summary = buildSummary(cause, derived)
}

// failedBelow reports whether any descendant of n failed.
//
// This is what separates an originating failure from an inherited one: a caller
// whose callee failed is reporting a consequence, while a failure with nothing
// broken beneath it had no callee to inherit from and is therefore the origin.
//
// Iterative for the same reason as walk: trace depth comes from the network and
// must not be able to exhaust the stack.
func failedBelow(n *SpanNode) bool {
	stack := append([]*SpanNode(nil), n.Children...)
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c.Failed() {
			return true
		}
		stack = append(stack, c.Children...)
	}
	return false
}

// buildSummary composes the one-line incident description (§9B.1).

// Pure string templating over fields already on the spans. Deterministic, free,
// and available even when nothing else about the trace is understood — which is
// the point: the value is in reliably stating known facts, not in inferring
// anything.
func buildSummary(cause *SpanNode, derived []*SpanNode) string {
	var b strings.Builder

	service := cause.Service
	if service == "" {
		service = "an unreported service"
	}
	fmt.Fprintf(&b, "%s failed at %s %s", service, cause.Method, cause.Route)

	details := make([]string, 0, 3)
	if cause.Status != 0 {
		details = append(details, fmt.Sprintf("%d", cause.Status))
	}
	if cause.Error != "" {
		details = append(details, cause.Error)
	}
	if cause.DurationMs > 0 {
		details = append(details, fmt.Sprintf("after %.0fms", cause.DurationMs))
	}
	if len(details) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(details, ", "))
	}
	b.WriteByte('.')

	// Only distinct *services* are named, not spans: a fan-out where one
	// service reported eight failing spans is one affected service, and
	// saying "8 downstream services also failed" would overstate the blast
	// radius of the incident being summarized.
	if len(derived) > 0 {
		seen := make(map[string]struct{}, len(derived))
		names := make([]string, 0, len(derived))
		for _, n := range derived {
			if n.Service == "" || n.Service == cause.Service {
				continue
			}
			if _, dup := seen[n.Service]; dup {
				continue
			}
			seen[n.Service] = struct{}{}
			names = append(names, n.Service)
		}
		if len(names) > 0 {
			sort.Strings(names)
			noun := "services"
			verb := "also failed as a result"
			if len(names) == 1 {
				noun = "service"
			}
			fmt.Fprintf(&b, " %d downstream %s (%s) %s.",
				len(names), noun, strings.Join(names, ", "), verb)
		}
	}
	return b.String()
}

// walk visits every node depth-first, passing each node with its parent.
//
// Iterative rather than recursive: depth is bounded only by what services
// reported, and a pathological or malicious trace should not be able to exhaust
// the goroutine stack of the process serving the dashboard.
func walk(root *SpanNode, visit func(parent, child *SpanNode)) {
	type frame struct{ parent, node *SpanNode }
	stack := []frame{{nil, root}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		visit(f.parent, f.node)
		for _, c := range f.node.Children {
			stack = append(stack, frame{f.node, c})
		}
	}
}

// TraceSummary is one row in the trace list (§9.2).
//
// A separate, flat type rather than a Trace with its spans omitted, because the
// list renders hundreds of rows and assembling a tree per row — to then discard
// it — would make listing traces cost more than opening one.
type TraceSummary struct {
	TraceID      string   `json:"trace_id"`
	Services     []string `json:"services"`
	RootService  string   `json:"root_service"`
	Route        string   `json:"route"`
	Method       string   `json:"method"`
	Status       int      `json:"status"`
	StartNanoUTC int64    `json:"start_ns"`
	DurationMs   float64  `json:"duration_ms"`
	SpanCount    int      `json:"span_count"`
	HasError     bool     `json:"has_error"`
	SkewFlagged  bool     `json:"skew_flagged,omitempty"`
}

func (s TraceSummary) hasService(name string) bool {
	for _, svc := range s.Services {
		if svc == name {
			return true
		}
	}
	return false
}

// summarize builds a TraceSummary directly from stored spans.
//
// Caller must hold the shard's read lock. Deliberately does not call Assemble:
// the tree, skew flags, and root-cause walk are all irrelevant to a list row,
// and this is the function that runs hundreds of times per dashboard poll.
func summarize(e *traceEntry) TraceSummary {
	sum := TraceSummary{
		TraceID:   e.id,
		SpanCount: len(e.spans),
	}
	if len(e.spans) == 0 {
		return sum
	}

	seen := make(map[string]struct{}, 4)
	var minStart, maxEnd int64

	// root is the reported entry point; earliest is the fallback used only when
	// no entry point was reported at all.
	var root, earliest *fleet.Span
	for i := range e.spans {
		sp := &e.spans[i]
		if sp.Service != "" {
			if _, dup := seen[sp.Service]; !dup {
				seen[sp.Service] = struct{}{}
				sum.Services = append(sum.Services, sp.Service)
			}
		}

		end := sp.StartNanoUTC + int64(sp.DurationMs*float64(1e6))
		if i == 0 || sp.StartNanoUTC < minStart {
			minStart = sp.StartNanoUTC
		}
		if i == 0 || end > maxEnd {
			maxEnd = end
		}
		if sp.Failed() {
			sum.HasError = true
		}
		// Prefer a real root — the parentless span, which is the request's
		// actual entry point — and fall back to the earliest span only when
		// no root was reported at all, so an orphaned trace still names a
		// service and route rather than rendering as a blank row.
		//
		// The two clauses must stay ordered this way round, and the fallback
		// must compare against `earliest` rather than `minStart`. An earlier
		// version tested `i == 0 || sp.StartNanoUTC < minStart` *after*
		// minStart had already been updated for this span, which is always
		// true at i == 0 and therefore pinned `root` to whichever span
		// happened to arrive first. That left the real-root check above it
		// dead code, and made RootService a function of batch arrival order:
		// the same trace reported gateway as its root when the gateway's
		// batch flushed first, and orders-service when it didn't.
		if sp.IsRoot() {
			if root == nil || !root.IsRoot() || sp.StartNanoUTC < root.StartNanoUTC {
				root = sp
			}
			continue
		}
		if earliest == nil || sp.StartNanoUTC < earliest.StartNanoUTC {
			earliest = sp
		}
	}
	if root == nil {
		root = earliest
	}

	sort.Strings(sum.Services)
	sum.StartNanoUTC = minStart
	sum.DurationMs = float64(maxEnd-minStart) / 1e6
	if root != nil {
		sum.RootService = root.Service
		sum.Route = root.Route
		sum.Method = root.Method
		sum.Status = root.Status
	}
	return sum
}
