//go:build linux

package video

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStatRejectsFIFOWithoutBlocking(t *testing.T) {
	m, root := testMount(t, nil, nil)
	pipe := filepath.Join(root, "pipe.mp4")
	if err := unix.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := m.stat("pipe.mp4")
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("FIFO error = %v, want ErrNotFound", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stat blocked while opening a FIFO")
	}

	_ = os.Remove(pipe)
}
