package dashboard

import (
	"sync/atomic"
	"time"
)

// reqIDCounter is a monotonic counter for request IDs.
//
// We use this instead of crypto/rand because crypto/rand.Read does a
// syscall to /dev/urandom on every call — at 130K req/s that's 130K
// syscalls/sec just for ID generation, which destroys throughput.
//
// The ID is encoded as a hex string with a timestamp prefix for sortability.
var reqIDCounter uint64

// newID returns a short, unique, sortable ID using an atomic counter.
//
// Format: 16 hex chars = 8 bytes:
//   - 4 bytes: seconds since epoch (gives sortability + uniqueness across restarts)
//   - 4 bytes: monotonic counter
//
// The counter used to be 2 bytes, which the comment above called enough for "more than
// 65535 requests in the same second". It is not: the counter wraps at 65536, so a service
// handling more than that per second (the dashboard is built for 130K) issued the same ID to
// two different requests in the same second, and a timeline, a log line or a request record
// looked up by that ID could answer with the wrong one. Four bytes would need more than four
// billion requests in one second to repeat.
//
// This is ~100x faster than crypto/rand.Read and allocation-free.
func newID() string {
	ts := uint64(time.Now().Unix()) & 0xFFFFFFFF
	seq := atomic.AddUint64(&reqIDCounter, 1) & 0xFFFFFFFF
	combined := (ts << 32) | seq
	var buf [16]byte
	const hexDigits = "0123456789abcdef"
	for i := 15; i >= 0; i-- {
		buf[i] = hexDigits[combined&0xF]
		combined >>= 4
	}
	return string(buf[:])
}
