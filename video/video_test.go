package video

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bufSink captures written bytes so a response can be asserted on without a
// socket. It also records each write separately, which is how the chunking
// tests verify that a large body really is streamed rather than buffered.
type bufSink struct {
	writes [][]byte
	failAt int // 1-based write index to fail on; 0 never fails
	n      int
}

func (b *bufSink) write(p []byte) error {
	b.n++
	if b.failAt != 0 && b.n >= b.failAt {
		return errors.New("connection reset by peer")
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	b.writes = append(b.writes, cp)
	return nil
}

func (b *bufSink) all() string {
	var sb strings.Builder
	for _, w := range b.writes {
		sb.Write(w)
	}
	return sb.String()
}

// body returns everything after the head terminator.
func (b *bufSink) body() string {
	s := b.all()
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		return s[i+4:]
	}
	return ""
}

// headerValue extracts a single header from the captured head.
func (b *bufSink) headerValue(key string) string {
	s := b.all()
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		s = s[:i]
	}
	for _, line := range strings.Split(s, "\r\n")[1:] {
		if k, v, ok := strings.Cut(line, ": "); ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func (b *bufSink) status() string {
	s := b.all()
	if i := strings.Index(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// testMount builds a mount over a temp directory containing the given
// files. Telemetry is disabled so tests do not touch package-level
// singletons and cannot interfere with one another.
func testMount(t *testing.T, files map[string]string, tweak func(*Config)) (*mount, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	cfg := Config{
		Root:                 root,
		DisableEvents:        true,
		DisableObservability: true,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	m, err := newMount(cfg)
	if err != nil {
		t.Fatalf("newMount: %v", err)
	}
	return m, root
}

func TestParseRange(t *testing.T) {
	const size = 1000
	tests := []struct {
		name    string
		header  string
		want    *byteRange
		wantErr error
	}{
		{"absent", "", nil, nil},
		{"full prefix", "bytes=0-499", &byteRange{0, 499}, nil},
		{"open ended", "bytes=500-", &byteRange{500, 999}, nil},
		{"suffix", "bytes=-200", &byteRange{800, 999}, nil},
		{"suffix longer than file", "bytes=-5000", &byteRange{0, 999}, nil},
		{"clamped past eof", "bytes=900-99999", &byteRange{900, 999}, nil},
		{"single byte", "bytes=0-0", &byteRange{0, 0}, nil},
		{"last byte", "bytes=999-999", &byteRange{999, 999}, nil},
		{"case insensitive unit", "BYTES=0-9", &byteRange{0, 9}, nil},
		{"whitespace", "bytes= 0 - 9 ", &byteRange{0, 9}, nil},
		{"multi range takes first", "bytes=0-99,200-299", &byteRange{0, 99}, nil},

		// Malformed values must be ignored (RFC 9110 §14.2), producing a
		// normal 200 rather than an error status.
		{"no dash", "bytes=100", nil, nil},
		{"not a number", "bytes=abc-def", nil, nil},
		{"inverted", "bytes=500-100", nil, nil},
		{"bare dash", "bytes=-", nil, nil},
		{"unknown unit", "items=0-10", nil, nil},
		{"no unit", "0-10", nil, nil},
		{"negative start", "bytes=--5", nil, nil},

		// Well-formed but unsatisfiable must be 416.
		{"start past eof", "bytes=1000-", nil, ErrRangeNotSatisfiable},
		{"far past eof", "bytes=5000-6000", nil, ErrRangeNotSatisfiable},
		{"zero suffix", "bytes=-0", nil, ErrRangeNotSatisfiable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRange(tc.header, size)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %+v, want nil", got)
			case tc.want != nil && got == nil:
				t.Fatalf("got nil, want %+v", tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("got %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

func TestParseRangeEmptyFile(t *testing.T) {
	// No range can be satisfied against zero bytes, and the distinction
	// matters: returning a 200 with an empty body would leave a client
	// believing it had received the requested span.
	if _, err := parseRange("bytes=0-0", 0); !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("err = %v, want ErrRangeNotSatisfiable", err)
	}
	// An absent header on an empty file is still a valid whole-file request.
	if r, err := parseRange("", 0); r != nil || err != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", r, err)
	}
}

func TestByteRangeContentRange(t *testing.T) {
	got := byteRange{0, 1023}.contentRange(146515)
	if want := "bytes 0-1023/146515"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestClamp(t *testing.T) {
	r := byteRange{0, 9999}
	got, changed := r.clamp(1000)
	if !changed || got.Length() != 1000 || got.Start != 0 || got.End != 999 {
		t.Fatalf("got %+v changed=%v", got, changed)
	}
	// A cap of zero means "no cap" and must leave the range untouched,
	// which is what MaxChunkSize < 0 configures.
	if got, changed := r.clamp(0); changed || got != r {
		t.Fatalf("cap 0 altered the range: %+v", got)
	}
}

func TestNormalizeRejectsTraversal(t *testing.T) {
	m, _ := testMount(t, map[string]string{"ok.mp4": "x"}, nil)

	hostile := []string{
		"../secret.mp4",
		"../../etc/passwd.mp4",
		"a/../../b.mp4",
		"%2e%2e%2fsecret.mp4",
		"%2e%2e/secret.mp4",
		"..%2fsecret.mp4",
		"....//secret.mp4",
		"a\\..\\b.mp4",
		"a/b/../../../c.mp4",
	}
	for _, raw := range hostile {
		t.Run(raw, func(t *testing.T) {
			if name, err := m.normalize(raw); err == nil {
				t.Fatalf("accepted hostile path %q as %q", raw, name)
			}
		})
	}
}

func TestNormalizeDoubleEncodedTraversal(t *testing.T) {
	m, _ := testMount(t, nil, nil)
	// A single unescape turns %252e into %2e, not into a dot, so this must
	// NOT become a traversal. It should fail as an unknown extension or a
	// missing file, never resolve above the root.
	if name, err := m.normalize("%252e%252e%252fsecret.mp4"); err == nil {
		if strings.Contains(name, "..") {
			t.Fatalf("double-encoded traversal survived as %q", name)
		}
	}
}

func TestNormalizeRejectsNUL(t *testing.T) {
	m, _ := testMount(t, nil, nil)
	if _, err := m.normalize("ok.mp4\x00.txt"); !errors.Is(err, ErrForbiddenPath) {
		t.Fatalf("err = %v, want ErrForbiddenPath", err)
	}
}

func TestNormalizeHiddenFiles(t *testing.T) {
	m, _ := testMount(t, nil, nil)
	for _, raw := range []string{".env.mp4", ".git/config.mp4", "a/.secret/b.mp4"} {
		if _, err := m.normalize(raw); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q: err = %v, want ErrNotFound", raw, err)
		}
	}

	// With AllowHidden the same paths pass normalization.
	open, _ := testMount(t, nil, func(c *Config) { c.AllowHidden = true })
	if _, err := open.normalize(".env.mp4"); err != nil {
		t.Fatalf("AllowHidden still rejected: %v", err)
	}
}

func TestNormalizeExtensionAllowList(t *testing.T) {
	m, _ := testMount(t, nil, nil)
	if _, err := m.normalize("script.php"); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("err = %v, want ErrUnsupportedType", err)
	}
	// Case must not be a bypass.
	if _, err := m.normalize("movie.MP4"); err != nil {
		t.Fatalf("uppercase extension rejected: %v", err)
	}
}

func TestStatRejectsDirectory(t *testing.T) {
	m, root := testMount(t, nil, nil)
	if err := os.MkdirAll(filepath.Join(root, "dir.mp4"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.stat("dir.mp4"); !errors.Is(err, ErrDirectory) {
		t.Fatalf("err = %v, want ErrDirectory", err)
	}
}

func TestStatRejectsSymlinkByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	m, root := testMount(t, map[string]string{"real.mp4": "data"}, nil)
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.mp4")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := m.stat("link.mp4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStatSymlinkEscapingRootStillRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	// Even with FollowSymlinks enabled, a link whose target leaves the
	// root must not be served: following links is a convenience, not
	// permission to escape.
	m, root := testMount(t, nil, func(c *Config) { c.FollowSymlinks = true })
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.mp4")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := m.stat("link.mp4"); !errors.Is(err, ErrForbiddenPath) {
		t.Fatalf("err = %v, want ErrForbiddenPath", err)
	}
}

func TestContainedRejectsSiblingPrefix(t *testing.T) {
	sep := string(os.PathSeparator)
	root := sep + "srv" + sep + "media"
	if contained(root, root+"-private"+sep+"x.mp4") {
		t.Fatal("sibling directory sharing a name prefix was treated as contained")
	}
	if !contained(root, root+sep+"x.mp4") {
		t.Fatal("real child rejected")
	}
}

func TestNewMountValidation(t *testing.T) {
	if _, err := newMount(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty Root: err = %v", err)
	}
	if _, err := newMount(
		Config{Root: filepath.Join(t.TempDir(), "nope")},
	); !errors.Is(
		err,
		ErrInvalidConfig,
	) {
		t.Fatalf("missing Root: err = %v", err)
	}
	// A file where a directory is expected must fail at setup, not per
	// request.
	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newMount(Config{Root: f}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("file as Root: err = %v", err)
	}
	if _, err := newMount(
		Config{Root: t.TempDir(), Prefix: "videos"},
	); !errors.Is(
		err,
		ErrInvalidConfig,
	) {
		t.Fatalf("prefix without slash: err = %v", err)
	}
}

func TestMountDefaults(t *testing.T) {
	m, _ := testMount(t, nil, nil)
	if m.prefix != "/videos" {
		t.Fatalf("prefix = %q", m.prefix)
	}
	if m.chunk != DefaultChunkSize {
		t.Fatalf("chunk = %d", m.chunk)
	}
	if m.maxChunk != DefaultMaxChunkSize {
		t.Fatalf("maxChunk = %d", m.maxChunk)
	}
	if m.cache != DefaultCacheControl {
		t.Fatalf("cache = %q", m.cache)
	}
	// "-" is the documented way to suppress the header entirely.
	off, _ := testMount(t, nil, func(c *Config) { c.CacheControl = "-" })
	if off.cache != "" {
		t.Fatalf("cache = %q, want empty", off.cache)
	}
	// A negative cap means "no cap".
	nocap, _ := testMount(t, nil, func(c *Config) { c.MaxChunkSize = -1 })
	if nocap.maxChunk != 0 {
		t.Fatalf("maxChunk = %d, want 0", nocap.maxChunk)
	}
}

func TestContentTypeFor(t *testing.T) {
	cases := map[string]string{
		"a.mp4":  "video/mp4",
		"a.webm": "video/webm",
		"a.m3u8": "application/vnd.apple.mpegurl",
		"a.ts":   "video/mp2t",
		"a.MP4":  "video/mp4",
		"a.xyz":  "application/octet-stream",
	}
	for name, want := range cases {
		if got := contentTypeFor(name); got != want {
			t.Fatalf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestETagAndConditionals(t *testing.T) {
	tag := etagFor(1000, 1700000000)
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Fatalf("etag %q is not quoted", tag)
	}
	// Different content must produce a different validator.
	if tag == etagFor(1001, 1700000000) || tag == etagFor(1000, 1700000001) {
		t.Fatal("etag does not vary with size and mtime")
	}

	if !etagMatches(tag, tag) {
		t.Fatal("exact match failed")
	}
	if !etagMatches("W/"+tag, tag) {
		t.Fatal("weak comparison failed")
	}
	if !etagMatches("*", tag) {
		t.Fatal("star should match any representation")
	}
	if !etagMatches(`"other", `+tag, tag) {
		t.Fatal("list match failed")
	}
	if etagMatches(`"other"`, tag) {
		t.Fatal("unrelated tag matched")
	}

	// If-None-Match takes precedence over If-Modified-Since, so a
	// mismatched tag must force a fresh response even when the date says
	// the cached copy is current.
	if notModified(`"stale"`, httpTime(1700000000), tag, 1700000000) {
		t.Fatal("stale etag was treated as fresh")
	}
	if !notModified("", httpTime(1700000000), tag, 1700000000) {
		t.Fatal("equal dates should be not-modified")
	}
	if notModified("", httpTime(1699999999), tag, 1700000000) {
		t.Fatal("newer file was treated as unmodified")
	}
	if notModified("", "not a date", tag, 1700000000) {
		t.Fatal("unparseable date should be ignored")
	}
}

func TestHeadBytesStripsCRLF(t *testing.T) {
	// A filename containing CRLF must not be able to inject headers.
	h := newHead(200).set("Content-Disposition", "inline; filename=\"a\r\nX-Evil: 1\"")
	got := string(h.bytes())
	if strings.Contains(got, "X-Evil: 1\r\n") && strings.Count(got, "\r\n\r\n") > 1 {
		t.Fatalf("header injection succeeded:\n%s", got)
	}
	if strings.Contains(got, "a\r\nX-Evil") {
		t.Fatalf("CRLF survived sanitisation:\n%s", got)
	}
}

func TestHeadOmitsEmptyValues(t *testing.T) {
	h := newHead(200).set("Cache-Control", "").set("ETag", `"x"`)
	got := string(h.bytes())
	if strings.Contains(got, "Cache-Control") {
		t.Fatalf("empty header emitted:\n%s", got)
	}
	if !strings.Contains(got, `ETag: "x"`) {
		t.Fatalf("expected header missing:\n%s", got)
	}
}

func TestSignRoundTrip(t *testing.T) {
	secret := []byte("s3cret")
	fixed := time.Unix(1700000000, 0)
	m, _ := testMount(t, nil, func(c *Config) {
		c.Secret = secret
		c.Clock = func() time.Time { return fixed }
	})

	q := SignAt(secret, "a/b.mp4", fixed.Add(time.Hour))
	exp, sig := parseQuery(t, q)

	if err := m.verifySignature("a/b.mp4", exp, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Changing the file must invalidate the signature, or one link would
	// grant access to the whole library.
	if err := m.verifySignature("a/other.mp4", exp, sig); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
	// Changing the expiry must invalidate it too.
	if err := m.verifySignature(
		"a/b.mp4",
		"9999999999",
		sig,
	); !errors.Is(
		err,
		ErrInvalidSignature,
	) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
	if err := m.verifySignature("a/b.mp4", "", ""); !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("err = %v, want ErrSignatureRequired", err)
	}
	if err := m.verifySignature(
		"a/b.mp4",
		"notanumber",
		sig,
	); !errors.Is(
		err,
		ErrInvalidSignature,
	) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
}

func TestSignExpiry(t *testing.T) {
	secret := []byte("s3cret")
	issued := time.Unix(1700000000, 0)
	// Clock is one second past the expiry.
	m, _ := testMount(t, nil, func(c *Config) {
		c.Secret = secret
		c.Clock = func() time.Time { return issued.Add(61 * time.Second) }
	})
	q := SignAt(secret, "a.mp4", issued.Add(60*time.Second))
	exp, sig := parseQuery(t, q)
	if err := m.verifySignature("a.mp4", exp, sig); !errors.Is(err, ErrSignatureExpired) {
		t.Fatalf("err = %v, want ErrSignatureExpired", err)
	}
}

func TestSignDisabledWhenNoSecret(t *testing.T) {
	m, _ := testMount(t, nil, nil)
	if err := m.verifySignature("a.mp4", "", ""); err != nil {
		t.Fatalf("unsigned mount demanded a signature: %v", err)
	}
}

func TestCanonicalNameMatchesResolver(t *testing.T) {
	// The name Sign canonicalises must equal the one normalize produces,
	// or a signature made by the caller would never verify. Dot-dot is
	// excluded here because normalize now refuses it outright, so there
	// is no canonical form for it to agree on.
	m, _ := testMount(t, nil, nil)
	for _, raw := range []string{"a/b.mp4", "/a/b.mp4", "a//b.mp4", "./a/b.mp4"} {
		got, err := m.normalize(raw)
		if err != nil {
			t.Fatalf("normalize(%q): %v", raw, err)
		}
		if want := canonicalName(raw); got != want {
			t.Fatalf("%q: normalize gave %q, canonicalName gave %q", raw, got, want)
		}
	}
}

func TestNormalizeRejectsDotDotEvenWhenHarmless(t *testing.T) {
	// "a/../a.mp4" would collapse to a path inside the root, so serving
	// it would be safe — but it is refused so that the attempt is visible
	// rather than silently rewritten.
	m, _ := testMount(t, nil, nil)
	if _, err := m.normalize("a/../a.mp4"); !errors.Is(err, ErrForbiddenPath) {
		t.Fatalf("err = %v, want ErrForbiddenPath", err)
	}
}

func TestStatusFor(t *testing.T) {
	cases := map[error]int{
		nil:                      200,
		ErrNotFound:              404,
		ErrForbiddenPath:         404, // must not reveal that it was blocked
		ErrDirectory:             404,
		ErrUnsupportedType:       404,
		ErrRangeNotSatisfiable:   416,
		ErrSignatureRequired:     403,
		ErrInvalidSignature:      403,
		ErrSignatureExpired:      403,
		errors.New("unexpected"): 500,
	}
	for err, want := range cases {
		if got := statusFor(err); got != want {
			t.Fatalf("%v: got %d, want %d", err, got, want)
		}
	}
	// Wrapped errors must keep their status.
	if got := statusFor(fmt.Errorf("context: %w", ErrForbiddenPath)); got != 404 {
		t.Fatalf("wrapped: got %d, want 404", got)
	}
}

func TestIsPeerGone(t *testing.T) {
	if isPeerGone(nil) {
		t.Fatal("nil reported as disconnect")
	}
	if !isPeerGone(errors.New("write tcp: broken pipe")) {
		t.Fatal("broken pipe not recognised")
	}
	if !isPeerGone(errors.New("connection reset by peer")) {
		t.Fatal("reset not recognised")
	}
	if isPeerGone(errors.New("permission denied")) {
		t.Fatal("unrelated error treated as disconnect")
	}
}

func TestCopyRangeChunks(t *testing.T) {
	// A body larger than the chunk size must arrive in several writes,
	// which is the property that keeps a 4 GiB file from being buffered.
	const size = 10000
	content := strings.Repeat("abcdefghij", size/10)
	m, root := testMount(t, map[string]string{"big.mp4": content}, func(c *Config) {
		c.ChunkSize = 1000
	})
	res, err := m.stat("big.mp4")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	_ = root

	out := &bufSink{}
	sent, err := m.writeBody(out, res, byteRange{0, size - 1})
	if err != nil {
		t.Fatalf("writeBody: %v", err)
	}
	if sent != size {
		t.Fatalf("sent %d, want %d", sent, size)
	}
	if len(out.writes) != 10 {
		t.Fatalf("got %d writes, want 10 chunks", len(out.writes))
	}
	if out.all() != content {
		t.Fatal("streamed bytes differ from file contents")
	}
}

func TestCopyRangeOffset(t *testing.T) {
	content := "0123456789"
	m, _ := testMount(t, map[string]string{"a.mp4": content}, nil)
	res, err := m.stat("a.mp4")
	if err != nil {
		t.Fatal(err)
	}
	out := &bufSink{}
	sent, err := m.writeBody(out, res, byteRange{3, 6})
	if err != nil {
		t.Fatalf("writeBody: %v", err)
	}
	if sent != 4 || out.all() != "3456" {
		t.Fatalf("sent %d bytes %q", sent, out.all())
	}
}

func TestCopyRangeReportsPartialOnDisconnect(t *testing.T) {
	// When the peer vanishes mid-stream the bytes already sent must be
	// reported, so a 90%-complete transfer is distinguishable from one
	// that never started.
	content := strings.Repeat("x", 5000)
	m, _ := testMount(t, map[string]string{"a.mp4": content}, func(c *Config) {
		c.ChunkSize = 1000
	})
	res, err := m.stat("a.mp4")
	if err != nil {
		t.Fatal(err)
	}
	out := &bufSink{failAt: 3}
	sent, err := m.writeBody(out, res, byteRange{0, 4999})
	if err == nil {
		t.Fatal("expected a write error")
	}
	if !isPeerGone(err) {
		t.Fatalf("error not classified as a disconnect: %v", err)
	}
	if sent != 2000 {
		t.Fatalf("sent = %d, want 2000 (two chunks before failure)", sent)
	}
}

func TestWriteErrorSuppressedAfterBody(t *testing.T) {
	// Once bytes are on the wire an error response must not be appended:
	// the client would read it as part of the video.
	m, _ := testMount(t, nil, nil)
	cs := &connSink{wrote: true}
	if err := m.writeError(cs, "", 404, 0); err != nil {
		t.Fatalf("writeError: %v", err)
	}
}

func TestWriteError416CarriesContentRange(t *testing.T) {
	m, _ := testMount(t, nil, nil)
	out := &bufSink{}
	if err := m.writeError(out, "", 416, 1234); err != nil {
		t.Fatal(err)
	}
	if got := out.headerValue("Content-Range"); got != "bytes */1234" {
		t.Fatalf("Content-Range = %q", got)
	}
	if !strings.Contains(out.status(), "416") {
		t.Fatalf("status = %q", out.status())
	}
}

func TestApplyCORS(t *testing.T) {
	// Disabled by default: no headers at all.
	off, _ := testMount(t, nil, nil)
	h := newHead(200)
	off.applyCORS(h, "https://a.example")
	if strings.Contains(string(h.bytes()), "Access-Control") {
		t.Fatal("CORS headers emitted without configuration")
	}

	// Explicit allow-list: only the listed origin is echoed, and Vary is
	// always present so caches do not cross-serve.
	list, _ := testMount(t, nil, func(c *Config) {
		c.AllowedOrigins = []string{"https://a.example"}
	})
	h = newHead(200)
	list.applyCORS(h, "https://a.example")
	s := string(h.bytes())
	if !strings.Contains(s, "Access-Control-Allow-Origin: https://a.example") {
		t.Fatalf("allowed origin not echoed:\n%s", s)
	}
	if !strings.Contains(s, "Vary: Origin") {
		t.Fatalf("Vary: Origin missing:\n%s", s)
	}

	h = newHead(200)
	list.applyCORS(h, "https://evil.example")
	s = string(h.bytes())
	if strings.Contains(s, "Allow-Origin") {
		t.Fatalf("disallowed origin echoed:\n%s", s)
	}
	if !strings.Contains(s, "Vary: Origin") {
		t.Fatalf("Vary must be sent even on refusal:\n%s", s)
	}

	// Wildcard.
	any, _ := testMount(t, nil, func(c *Config) { c.AllowedOrigins = []string{"*"} })
	h = newHead(200)
	any.applyCORS(h, "https://whatever.example")
	if !strings.Contains(string(h.bytes()), "Access-Control-Allow-Origin: *") {
		t.Fatalf("wildcard not honoured:\n%s", h.bytes())
	}
}

// parseQuery pulls exp and sig out of an encoded query string.
func parseQuery(t *testing.T, q string) (exp, sig string) {
	t.Helper()
	for _, pair := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(pair, "=")
		switch k {
		case "exp":
			exp = v
		case "sig":
			sig = v
		}
	}
	if exp == "" || sig == "" {
		t.Fatalf("query %q missing exp or sig", q)
	}
	return exp, sig
}
