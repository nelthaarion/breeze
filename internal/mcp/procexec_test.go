package mcp

// procexec_test.go — what a child process inherits, and what it can make the server hold.
//
// The helper-process pattern is used so the tests run the same way on Linux and Windows:
// the test binary re-executes itself in a mode selected by an environment variable and
// plays the part of `go test`, a flooding test, or a hung one.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const helperModeEnv = "BREEZE_MCP_TEST_HELPER_MODE"

// TestHelperProcess is not a test: it is the body of the child the tests below start.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("helper process; started by the tests in procexec_test.go")
	}
	switch mode {
	case "env":
		fmt.Printf("token=[%s] other=[%s]\n", os.Getenv("BREEZE_MCP_TOKEN"), os.Getenv("BREEZE_TEST_KEEP"))
	case "flood":
		line := strings.Repeat("x", 99) + "\n"
		for i := 0; i < 200_000; i++ { // 20 MB
			fmt.Print(line)
		}
		fmt.Println("TAIL-MARKER-FAIL example.com/pkg 0.1s")
	case "hang-with-grandchild":
		// Like `go test` running a test binary: the child starts a grandchild that
		// inherits stdout, then neither finishes.
		self, _ := os.Executable()
		grand := exec.Command(self, "-test.run=^TestHelperProcess$")
		grand.Env = append(os.Environ(), helperModeEnv+"=sleep")
		grand.Stdout, grand.Stderr = os.Stdout, os.Stderr
		if err := grand.Start(); err != nil {
			fmt.Println("grandchild failed:", err)
			os.Exit(2)
		}
		fmt.Printf("grandchild=%d\n", grand.Process.Pid)
		time.Sleep(2 * time.Minute)
	case "sleep":
		time.Sleep(2 * time.Minute)
	}
	os.Exit(0)
}

func helperCmd(ctx context.Context, mode string, extraEnv ...string) *exec.Cmd {
	self, _ := os.Executable()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestHelperProcess$")
	cmd.Env = append(append(os.Environ(), helperModeEnv+"="+mode), extraEnv...)
	return cmd
}

func TestScrubEnvRemovesOnlyTheControlToken(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"BREEZE_MCP_TOKEN=s3cret",
		"breeze_mcp_token=also-dropped",
		"BREEZE_DATABASE_URL=postgres://u:p@h/db",
		"BREEZE_MCP_TOKEN_FILE=/not/the/token/itself",
		"HOME=/home/x",
	}
	got := scrubEnv(in)
	joined := strings.Join(got, "\n")
	for _, bad := range []string{"s3cret", "also-dropped"} {
		if strings.Contains(joined, bad) {
			t.Errorf("the control token survived scrubbing (%q): %v", bad, got)
		}
	}
	for _, keep := range []string{"PATH=/usr/bin", "BREEZE_DATABASE_URL=postgres://u:p@h/db", "HOME=/home/x", "BREEZE_MCP_TOKEN_FILE=/not/the/token/itself"} {
		if !strings.Contains(joined, keep) {
			t.Errorf("%q was dropped; only the control token should be", keep)
		}
	}
}

func TestChildDoesNotInheritTheControlToken(t *testing.T) {
	t.Setenv("BREEZE_MCP_TOKEN", "must-not-reach-the-child-0123456789")
	t.Setenv("BREEZE_TEST_KEEP", "kept")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := helperCmd(ctx, "env", "BREEZE_MCP_TOKEN=must-not-reach-the-child-0123456789")
	out := prepareChild(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out.String())
	}
	got := out.String()
	if strings.Contains(got, "must-not-reach-the-child") {
		t.Fatalf("the child could read the control token: %s", got)
	}
	if !strings.Contains(got, "token=[]") || !strings.Contains(got, "other=[kept]") {
		t.Errorf("unexpected child environment: %s", got)
	}
}

func TestExtraEnvOverridesWhatIsInherited(t *testing.T) {
	cmd := exec.Command("true")
	cmd.Env = []string{"GO111MODULE=off", "A=1"}
	prepareChild(cmd, "GO111MODULE=on")
	last := ""
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GO111MODULE=") {
			last = kv // the later entry is the one the child sees
		}
	}
	if last != "GO111MODULE=on" {
		t.Errorf("effective GO111MODULE = %q, want the forced value", last)
	}
}

func TestOutputCaptureKeepsSmallOutputVerbatim(t *testing.T) {
	c := newOutputCapture(64, 64)
	_, _ = c.Write([]byte("hello "))
	_, _ = c.Write([]byte("world\n"))
	if got := c.String(); got != "hello world\n" || c.Truncated() {
		t.Errorf("small output was altered: %q (truncated=%v)", got, c.Truncated())
	}
}

func TestOutputCaptureKeepsTheHeadAndTheTail(t *testing.T) {
	c := newOutputCapture(10, 10)
	_, _ = c.Write([]byte("HEAD-------"))                        // 11 bytes: 10 to head, 1 to tail
	_, _ = c.Write([]byte(strings.Repeat("m", 1000)))            // the middle, mostly dropped
	_, _ = c.Write([]byte("...the-final-summary: FAIL example")) // longer than the tail
	got := c.String()

	if !strings.HasPrefix(got, "HEAD------") {
		t.Errorf("the beginning was lost: %q", got)
	}
	if !strings.HasSuffix(got, "mary: FAIL example"[len("mary: FAIL example")-10:]) {
		t.Errorf("the end was lost: %q", got)
	}
	if !c.Truncated() || !strings.Contains(got, "bytes of output omitted") {
		t.Errorf("dropping output was not announced: %q", got)
	}
	if len(got) > 10+10+120 {
		t.Errorf("kept %d bytes from a 10+10 budget", len(got))
	}
}

func TestOutputCaptureTailIsExactAcrossWraps(t *testing.T) {
	c := newOutputCapture(4, 16)
	var all strings.Builder
	for i := 0; i < 500; i++ {
		piece := fmt.Sprintf("<%03d>", i)
		all.WriteString(piece)
		_, _ = c.Write([]byte(piece))
	}
	want := all.String()
	got := c.String()
	if !strings.HasPrefix(got, want[:4]) {
		t.Errorf("head wrong: %q", got)
	}
	if !strings.HasSuffix(got, want[len(want)-16:]) {
		t.Errorf("the last 16 bytes are not the last 16 written: got %q, want suffix %q", got, want[len(want)-16:])
	}
}

func TestAFloodingChildCannotExhaustMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := helperCmd(ctx, "flood")
	out := prepareChild(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper failed: %v", err)
	}
	got := out.String()
	if limit := childOutputHead + childOutputTail + 1024; len(got) > limit {
		t.Errorf("kept %d bytes of a 20 MB flood; the limit is %d", len(got), limit)
	}
	if !strings.Contains(got, "TAIL-MARKER-FAIL") {
		t.Error("the final line — where go test prints its verdict — was not kept")
	}
	if !out.Truncated() {
		t.Error("a 20 MB flood was not reported as truncated")
	}
}

func TestATimeoutReturnsEvenWhenAGrandchildHoldsTheOutput(t *testing.T) {
	// This is the shape of a hung `go test`: the process the server started is not the
	// one holding the pipe. Before, Wait blocked until the grandchild exited — which for a
	// hung test is never — so the timeout bounded nothing.
	old := childWaitDelay
	childWaitDelay = time.Second
	t.Cleanup(func() { childWaitDelay = old })

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	cmd := helperCmd(ctx, "hang-with-grandchild")
	out := prepareChild(cmd)

	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a killed child reported success")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Run took %v after a 750ms timeout: a grandchild holding the pipe held the call hostage", elapsed)
	}
	if ctx.Err() == nil {
		t.Error("the context did not expire; the test did not exercise the timeout")
	}

	if runtime.GOOS != "linux" {
		return
	}
	// On Linux the group kill should also have ended the grandchild.
	var pid int
	for _, line := range strings.Split(out.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "grandchild="); ok {
			pid, _ = strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	if pid == 0 {
		t.Skip("the helper did not report a grandchild pid")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return // gone
		}
		// "pid (comm) S ..." — a zombie awaiting reaping is dead for our purposes.
		if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("grandchild %d is still running after the timeout: the process group was not killed", pid)
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func TestChildSlotsBoundConcurrency(t *testing.T) {
	old := childSlots
	childSlots = make(chan struct{}, 2)
	t.Cleanup(func() { childSlots = old })

	var (
		mu      sync.Mutex
		running int
		peak    int
		wg      sync.WaitGroup
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquireChild(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			mu.Lock()
			running++
			if running > peak {
				peak = running
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if peak > 2 {
		t.Fatalf("%d children ran at once with 2 slots", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency was %d; the slots were not used in parallel", peak)
	}
}

func TestWaitingForASlotEndsWithTheContext(t *testing.T) {
	old := childSlots
	childSlots = make(chan struct{}, 1)
	t.Cleanup(func() { childSlots = old })

	release, err := acquireChild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := acquireChild(ctx); err == nil {
		t.Fatal("acquired a slot that was not free")
	}
	if time.Since(started) > 5*time.Second {
		t.Error("waiting for a slot ignored the context's deadline")
	}
}

func TestRunGoReportsABusyServerInsteadOfHanging(t *testing.T) {
	old := childSlots
	childSlots = make(chan struct{}, 1)
	t.Cleanup(func() { childSlots = old })
	childSlots <- struct{}{} // the only slot is taken

	run := runGo(t.TempDir(), 100*time.Millisecond, "version")
	if run.StartErr == nil || !strings.Contains(run.StartErr.Error(), "limit") {
		t.Fatalf("a busy server did not say so: %+v", run)
	}
}
