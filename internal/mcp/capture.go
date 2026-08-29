package mcp

// capture.go — running the generator without corrupting the protocol stream.
//
// The generator reports progress with 40-odd bare fmt.Print calls. Those go to
// os.Stdout, and on a stdio transport os.Stdout is the protocol stream: a single
// "Created models/user.go" landing between two JSON-RPC frames makes the session
// unparseable, and the peer's error would point at the framing rather than at the
// real cause.
//
// So os.Stdout is replaced with a pipe for the duration of a call, and what the
// generator printed is returned as the tool's result. The output stops being noise
// and becomes the answer: "Created models/user.go" is exactly what the caller
// wanted to know.
//
// The alternative was threading an io.Writer through every generator function.
// That is the better design and it is not what this does, because it would mean
// touching all 36 files for a refactor unrelated to the reason for the change, in
// the same commit. This confines the workaround to one file and one comment.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

// captureMu serialises captures.
//
// os.Stdout is process-global, so two concurrent captures would each restore
// whatever they happened to see, and one would restore the other's pipe as the
// real stdout — permanently. rpc dispatch can run handlers concurrently, so this
// is a real possibility rather than a theoretical one.
//
// Serialising also means a tool call is effectively single-threaded, which is
// correct for a different reason: the generators chdir, and chdir is per-process
// too.
var captureMu sync.Mutex

// captureStdout runs fn with os.Stdout redirected, returning what fn printed.
//
// The returned error is fn's own. A failure to set up the pipe is reported
// separately, because the two mean different things to a caller: one is a bad
// request, the other is a broken process.
func captureStdout(fn func() error) (string, error) {
	captureMu.Lock()
	defer captureMu.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("mcp: cannot create a pipe to capture output: %w", err)
	}

	saved := os.Stdout
	os.Stdout = w

	// The reader runs in its own goroutine because a pipe has a finite buffer:
	// if the generator prints more than that while nothing is draining, its
	// write blocks and the call deadlocks. Draining concurrently is what makes
	// the amount of output irrelevant.
	var (
		buf  bytes.Buffer
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		_, _ = io.Copy(&buf, r)
	}()

	// The restore is deferred so a panic in fn cannot leave os.Stdout pointing at
	// a pipe nobody reads. Without this, one panicking generator would silently
	// break every later response in the session.
	var (
		fnErr  error
		panicV any
	)
	func() {
		defer func() {
			panicV = recover()
			os.Stdout = saved
			_ = w.Close() // unblocks the reader
			<-done        // wait for the drain to finish before reading buf
			_ = r.Close()
		}()
		fnErr = fn()
	}()
	if panicV != nil {
		// Restoration has already happened. Re-panicking preserves the caller's
		// recovery contract instead of turning a programmer bug into a fake
		// success with whatever happened to be printed before it.
		panic(panicV)
	}

	return buf.String(), fnErr
}

// runInDir runs fn with the process working directory set to dir.
//
// The generators resolve paths relative to the working directory, which is what
// makes them reusable across commands but also means an MCP client — whose
// working directory is wherever it happened to start — has to be able to say
// where the project is.
//
// This is under the same lock as captureStdout for the same reason: chdir is
// process-global. Callers must not nest the two.
func runInDir(dir string, fn func() error) error {
	if dir == "" {
		return fn()
	}

	prev, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("mcp: cannot determine the current directory: %w", err)
	}

	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("mcp: cannot enter %s: %w", dir, err)
	}
	defer func() { _ = os.Chdir(prev) }()

	return fn()
}
