package video

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nullSink discards everything. It measures the cost of this package
// without the cost of a socket, which is what a benchmark should isolate.
type nullSink struct{ n int64 }

func (s *nullSink) write(p []byte) error {
	s.n += int64(len(p))
	return nil
}

// BenchmarkNormalize measures the pure validation path — the work done on
// every request before any syscall. It should allocate nothing for an
// ordinary name, because it runs on every request including hostile ones.
func BenchmarkNormalize(b *testing.B) {
	m, _ := benchMount(b, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.normalize("show/season-1/episode-01.mp4"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNormalizeHostile confirms that rejecting an attack is at least
// as cheap as accepting a real path; if it were not, the validator would
// itself be a denial-of-service vector.
func BenchmarkNormalizeHostile(b *testing.B) {
	m, _ := benchMount(b, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.normalize("%2e%2e%2f%2e%2e%2fetc/passwd"); err == nil {
			b.Fatal("expected rejection")
		}
	}
}

func BenchmarkParseRange(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseRange("bytes=1048576-2097151", 1<<30); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHeadBytes measures serialising a full 206 head, which happens
// once per seek.
func BenchmarkHeadBytes(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := newHead(206).
			set("Content-Type", "video/mp4").
			set("Accept-Ranges", "bytes").
			set("ETag", `"65f-1000"`).
			set("Last-Modified", "Tue, 14 Nov 2023 22:13:20 GMT").
			set("Cache-Control", DefaultCacheControl).
			set("Content-Range", "bytes 1048576-2097151/1073741824")
		h.setInt("Content-Length", 1048576)
		_ = h.bytes()
	}
}

func BenchmarkETag(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = etagFor(1073741824, 1700000000)
	}
}

// BenchmarkStreamChunk measures the streaming loop over a 4 MiB body: the
// per-request cost of the thing this package exists to do. Allocations
// here should be near zero per operation, since the read buffer is pooled.
func BenchmarkStreamChunk(b *testing.B) {
	const size = 4 << 20
	m, res := benchMount(b, size)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := &nullSink{}
		if _, err := m.writeBody(out, res, byteRange{0, size - 1}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamSeek measures the common player pattern: a small range
// read from the middle of a large file, which is what every scrub does.
func BenchmarkStreamSeek(b *testing.B) {
	const size = 4 << 20
	m, res := benchMount(b, size)
	r := byteRange{size / 2, size/2 + (256 << 10) - 1}
	b.SetBytes(r.Length())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := &nullSink{}
		if _, err := m.writeBody(out, res, r); err != nil {
			b.Fatal(err)
		}
	}
}

// benchMount builds a mount with one file of the given size and returns it
// already resolved, so the benchmark loop measures streaming rather than
// setup. A size of 0 creates no file.
func benchMount(b *testing.B, size int) (*mount, resolved) {
	b.Helper()
	root := b.TempDir()
	cfg := Config{
		Root:                 root,
		DisableEvents:        true,
		DisableObservability: true,
	}
	m, err := newMount(cfg)
	if err != nil {
		b.Fatalf("newMount: %v", err)
	}
	if size == 0 {
		return m, resolved{}
	}
	name := "bench.mp4"
	content := strings.Repeat("0123456789abcdef", size/16)
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		b.Fatal(err)
	}
	res, err := m.stat(name)
	if err != nil {
		b.Fatal(err)
	}
	return m, res
}
