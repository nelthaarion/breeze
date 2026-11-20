package mcp

// procexec.go — the limits every child process this package starts runs under.
//
// Two commands are run from here: the Go toolchain (gocmd.go) and the docker CLI
// (docker.go). Both execute things this server does not control — a project's tests,
// a Dockerfile's build steps — on behalf of a network caller, so what a child inherits
// and how much it can make this process hold are security properties, not conveniences.
//
// # What a child inherits
//
// Its environment is the server's own, minus the server's credentials. A project's
// tests are arbitrary code, often written by the same agent that asked for them to be
// run; with a token scoped to verification and nothing else, `os.Getenv` in a test is
// all it takes to read the variable the control plane's token came from. The token has
// no business in a child, so it is not passed.
//
// # What a child can make this process hold
//
//   - Output is captured through a bounded head-and-tail buffer. cmd.CombinedOutput
//     grows without limit, and a test that prints in a loop — by accident or on
//     purpose — is a way to exhaust the memory of a control plane that may be
//     embedded in a production process.
//   - A timeout kills the child's whole process group, and the wait for its output is
//     bounded. Killing only the direct child is not enough: `go test` runs the test
//     binary as a grandchild, which inherits the output pipe, and Wait does not return
//     until every holder of the pipe has closed it. A hung test therefore turned the
//     timeout meant to bound it into a call that never came back.

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// controlSecretEnv names the variables that carry this server's own credentials.
// Matched case-insensitively, because environment names are case-insensitive on Windows
// and a stray lower-case spelling is harmless to drop elsewhere.
var controlSecretEnv = []string{"BREEZE_MCP_TOKEN"}

// scrubEnv returns env without the server's own credentials.
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if isControlSecret(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isControlSecret(name string) bool {
	for _, secret := range controlSecretEnv {
		if strings.EqualFold(name, secret) {
			return true
		}
	}
	return false
}

// childSlots bounds how many child processes this server runs at once.
//
// Every call that reaches a child is a compiler or a test binary or a docker build, and a
// caller decides how many to make at the same time. Over HTTP each request is served on
// its own goroutine, so without a bound a client — or a bug in one — that fires a hundred
// verify_project calls starts a hundred `go test` runs and takes the host with it. The
// bound is the larger of four and the number of CPUs: enough that an ordinary agent never
// notices, little enough that a flood queues instead of running.
var childSlots = make(chan struct{}, childSlotCount())

func childSlotCount() int {
	if n := runtime.NumCPU(); n > 4 {
		return n
	}
	return 4
}

// acquireChild waits for a free slot, or for ctx to end, and returns the function that
// gives the slot back. The caller must call it exactly once, whatever the child did.
func acquireChild(ctx context.Context) (release func(), err error) {
	select {
	case childSlots <- struct{}{}:
		return func() { <-childSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// childOutputHead and childOutputTail bound what is kept of a child's output: the first
// 2 MiB and the last 6 MiB. The ends are what matter — a build's first errors and a test
// run's closing FAIL/ok summary — and a middle that was 40 MiB of the same line is not
// something a caller can use.
const (
	childOutputHead = 2 << 20
	childOutputTail = 6 << 20
)

// childWaitDelay is how long to wait for a killed child's output to drain before giving
// up on it. A variable so a test can shorten it.
var childWaitDelay = 5 * time.Second

// prepareChild applies the standard limits to cmd and returns what its output will be
// read from.
//
// cmd must have been made with exec.CommandContext, and cmd.Dir must already be set:
// the environment is derived from cmd.Environ(), which includes the PWD entry that
// follows from Dir. extraEnv is appended last, so it wins over anything inherited.
func prepareChild(cmd *exec.Cmd, extraEnv ...string) *outputCapture {
	cmd.Env = append(scrubEnv(cmd.Environ()), extraEnv...)
	cmd.WaitDelay = childWaitDelay
	setProcessGroup(cmd)

	out := newOutputCapture(childOutputHead, childOutputTail)
	// One writer for both streams, so exec uses a single pipe and the two interleave in
	// the order they were written — which is what makes a mixed build/test run readable.
	cmd.Stdout = out
	cmd.Stderr = out
	return out
}

// outputCapture is an io.Writer that keeps the beginning and the end of what it is given
// and counts what it dropped from between.
//
// A ring buffer rather than a slice that is trimmed, because trimming copies: with a
// 6 MiB tail and a child writing in small chunks, copying the tail on every write after
// the limit is quadratic.
type outputCapture struct {
	mu sync.Mutex

	headMax int
	head    []byte

	tailMax int
	ring    []byte // allocated on first use: most output never reaches the tail
	pos     int    // next write position in ring
	full    bool   // ring has wrapped at least once
	total   int64  // bytes ever written to the tail
}

func newOutputCapture(headMax, tailMax int) *outputCapture {
	return &outputCapture{headMax: headMax, tailMax: tailMax}
}

// Write implements io.Writer. It never fails and never blocks: a full buffer overwrites
// its oldest tail bytes rather than refusing, because refusing would stall the child.
func (o *outputCapture) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	n := len(p)
	if len(o.head) < o.headMax {
		take := o.headMax - len(o.head)
		if take > len(p) {
			take = len(p)
		}
		o.head = append(o.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}

	if o.ring == nil {
		o.ring = make([]byte, o.tailMax)
	}
	o.total += int64(len(p))
	// Only the last tailMax bytes of an oversized write can survive, so skip the rest
	// rather than copying bytes that the next line overwrites.
	if len(p) > o.tailMax {
		p = p[len(p)-o.tailMax:]
		o.full = true
		o.pos = 0
	}
	for len(p) > 0 {
		chunk := copy(o.ring[o.pos:], p)
		o.pos += chunk
		p = p[chunk:]
		if o.pos == o.tailMax {
			o.pos = 0
			o.full = true
		}
	}
	return n, nil
}

// String returns what was kept, with a marker where output was dropped. Output that fit
// is returned exactly as written.
func (o *outputCapture) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	var b strings.Builder
	b.Write(o.head)
	if o.ring == nil {
		return b.String()
	}

	kept := int64(o.pos)
	if o.full {
		kept = int64(o.tailMax)
	}
	if dropped := o.total - kept; dropped > 0 {
		b.WriteString("\n… [")
		b.WriteString(strconv.FormatInt(dropped, 10))
		b.WriteString(" bytes of output omitted: only the first and last parts are kept] …\n")
	}
	if o.full {
		b.Write(o.ring[o.pos:])
	}
	b.Write(o.ring[:o.pos])
	return b.String()
}

// Truncated reports whether any output was dropped.
func (o *outputCapture) Truncated() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ring == nil {
		return false
	}
	kept := int64(o.pos)
	if o.full {
		kept = int64(o.tailMax)
	}
	return o.total > kept
}
