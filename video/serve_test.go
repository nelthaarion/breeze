package video

// Tests that drive mount.serve — the function that turns a request into the
// bytes on the wire.
//
// Everything else in this package is tested through its own inputs: normalize
// takes a path, parseRange takes a header value, copyRange takes a file and a
// range. serve is what decides which of those runs, in what order, and what
// status comes out, and until now nothing called it. That gap is why the status
// codes documented in video/README.md could drift from the ones actually
// emitted — turning the unsolicited 206 into a 200, or dropping the
// Content-Range that tells a player the full duration, would have broken no
// test in this package.
//
// The seam that makes this testable without a socket is serve's explicit sink
// parameter: handler() passes a *connSink wrapping a gnet Conn, a test passes a
// bufSink. Requests are built with breeze.NewContext plus SetParam, which is
// what the router itself does for a "*filepath" wildcard.

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	breeze "github.com/nelthaarion/breeze"
)

// The body every test serves: 100 bytes of a repeating digit pattern, so an
// assertion can name the exact bytes a given range must return.
const (
	testBody = "0123456789012345678901234567890123456789" +
		"0123456789012345678901234567890123456789" +
		"01234567890123456789"
	testSize = 100
)

// serveResult is one call to serve: what it returned, and what it wrote.
type serveResult struct {
	sink   *bufSink
	name   string
	status int
	sent   int64
	err    error
}

// serveReqWith runs one request against m, writing into the given sink.
//
// target is the value the router would have captured for the wildcard,
// optionally carrying a query string ("clip.mp4?exp=…&sig=…"). It has no
// leading slash, because the router builds it with strings.Join over the
// remaining path segments (router.go:480); inventing one here would exercise a
// shape production never produces.
//
// Header keys are lowercased on the way in, because the request parser
// lowercases them and serve reads them only in that form. Passing "Range"
// through verbatim would let a test pass while production silently never
// matched — the failure serve's own comment warns about.
func serveReqWith(t *testing.T, m *mount, method breeze.Method, target string, headers map[string]string, out *bufSink) serveResult {
	t.Helper()

	raw, query := target, ""
	if i := strings.IndexByte(target, '?'); i >= 0 {
		raw, query = target[:i], target[i+1:]
	}

	ctx := breeze.NewContext(method, m.prefix+"/"+raw)
	ctx.SetParam("filepath", raw)
	if query != "" {
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatalf("bad query %q: %v", query, err)
		}
		ctx.Req.Query = q
	}
	for k, v := range headers {
		ctx.Req.Header[strings.ToLower(k)] = v
	}

	name, status, sent, err := m.serve(ctx, out)
	return serveResult{sink: out, name: name, status: status, sent: sent, err: err}
}

func serveReq(t *testing.T, m *mount, method breeze.Method, target string, headers map[string]string) serveResult {
	t.Helper()
	return serveReqWith(t, m, method, target, headers, &bufSink{})
}

func serveGET(t *testing.T, m *mount, target string, headers map[string]string) serveResult {
	t.Helper()
	return serveReq(t, m, breeze.GET, target, headers)
}

// bodyMount is the common fixture: one 100-byte clip, plus whatever config the
// test needs.
func bodyMount(t *testing.T, tweak func(*Config)) (*mount, string) {
	t.Helper()
	return testMount(t, map[string]string{"clip.mp4": testBody}, tweak)
}

// wantStatus checks the status serve reported *and* the status line it wrote.
// Either alone would miss a serve that returns 206 while writing 200: the
// returned value is what reaches observability, the written line is what
// reaches the client, and a mismatch between them is invisible from one side.
func (r serveResult) wantStatus(t *testing.T, code int) {
	t.Helper()
	if r.status != code {
		t.Errorf("serve returned status %d, want %d (err=%v)", r.status, code, r.err)
	}
	want := "HTTP/1.1 " + strconv.Itoa(code) + " " + httpReason[code]
	if got := r.sink.status(); got != want {
		t.Errorf("status line = %q, want %q", got, want)
	}
}

func (r serveResult) wantHeader(t *testing.T, key, want string) {
	t.Helper()
	if got := r.sink.headerValue(key); got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func (r serveResult) wantNoHeader(t *testing.T, key string) {
	t.Helper()
	if got := r.sink.headerValue(key); got != "" {
		t.Errorf("%s = %q, want it absent", key, got)
	}
}

// wantBody checks the bytes after the head terminator. It deliberately does not
// cross-check r.sent, because sent counts body bytes read from the *file* and
// an error response carries a reason phrase that never came from one.
func (r serveResult) wantBody(t *testing.T, want string) {
	t.Helper()
	if got := r.sink.body(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// wantSentBody is wantBody for a success path, where the body is file content
// and so must equal what serve reports having sent.
func (r serveResult) wantSentBody(t *testing.T, want string) {
	t.Helper()
	r.wantBody(t, want)
	if r.sent != int64(len(want)) {
		t.Errorf("serve reported %d bytes sent, wrote %d", r.sent, len(want))
	}
}

func (r serveResult) wantNoError(t *testing.T) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("serve returned an error: %v", r.err)
	}
}

// --- The status table in video/README.md -----------------------------------
//
// One test per documented row. These are the contract a player depends on, and
// each row is reached by a different path through serve.

// TestServeStatus200OnlyForEmptyFile pins the narrowest row: 200 is not "the
// success case" here, it is the empty-file case. Every non-empty success is a
// 206, which is what lets a player seek from the very first response.
func TestServeStatus200OnlyForEmptyFile(t *testing.T) {
	m, _ := testMount(t, map[string]string{"empty.mp4": ""}, nil)

	r := serveGET(t, m, "empty.mp4", nil)
	r.wantNoError(t)
	r.wantStatus(t, 200)
	r.wantHeader(t, "Content-Length", "0")
	r.wantSentBody(t, "")
	// A zero-length 200 must not claim a range: "bytes 0--1/0" is what the
	// End: -1 sentinel would render to if this branch ever emitted one.
	r.wantNoHeader(t, "Content-Range")
	r.wantHeader(t, "Accept-Ranges", "bytes")
}

// TestServeStatus206ForValidRange is the ordinary seek.
func TestServeStatus206ForValidRange(t *testing.T) {
	m, _ := bodyMount(t, nil)

	cases := []struct {
		header    string
		wantRange string
		wantBody  string
	}{
		{"bytes=10-19", "bytes 10-19/100", testBody[10:20]},
		{"bytes=0-0", "bytes 0-0/100", testBody[:1]},
		{"bytes=99-99", "bytes 99-99/100", testBody[99:]},
		// Open-ended: from N to EOF, which is what a player sends to resume.
		{"bytes=90-", "bytes 90-99/100", testBody[90:]},
		// Suffix: the last N bytes, which is how an MP4 moov atom at the end
		// of the file gets fetched before playback starts.
		{"bytes=-5", "bytes 95-99/100", testBody[95:]},
		// Past EOF is legal and clamps rather than failing.
		{"bytes=90-500", "bytes 90-99/100", testBody[90:]},
		{"bytes=-500", "bytes 0-99/100", testBody},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			r := serveGET(t, m, "clip.mp4", map[string]string{"range": tc.header})
			r.wantNoError(t)
			r.wantStatus(t, 206)
			r.wantHeader(t, "Content-Range", tc.wantRange)
			r.wantHeader(t, "Content-Length", strconv.Itoa(len(tc.wantBody)))
			r.wantHeader(t, "Content-Type", "video/mp4")
			r.wantSentBody(t, tc.wantBody)
		})
	}
}

// TestServeStatus206WithoutRange is the row most likely to be "corrected" by
// someone reading RFC 9110 in isolation: no Range legally permits a 200 with
// the whole representation, and this package deliberately does not do that.
// Sending the first chunk as an unsolicited 206 is what states the total size
// up front, so the player can seek instead of waiting out a whole movie.
func TestServeStatus206WithoutRange(t *testing.T) {
	// A chunk smaller than the file, so "first chunk" is observably not
	// "whole file".
	m, _ := bodyMount(t, func(c *Config) { c.ChunkSize = 32 })

	r := serveGET(t, m, "clip.mp4", nil)
	r.wantNoError(t)
	r.wantStatus(t, 206)
	// The size in Content-Range is the full 100, not the 32 that was sent.
	r.wantHeader(t, "Content-Range", "bytes 0-31/100")
	r.wantHeader(t, "Content-Length", "32")
	r.wantSentBody(t, testBody[:32])
}

// TestServeStatus206ForMalformedRange covers RFC 9110 §14.2: a Range that
// cannot be parsed is ignored, not rejected, so a broken proxy that mangles the
// header degrades to normal playback. Ignoring it lands on the no-Range branch,
// which in this package means 206 and the first chunk — not 200.
func TestServeStatus206ForMalformedRange(t *testing.T) {
	m, _ := bodyMount(t, func(c *Config) { c.ChunkSize = 32 })

	for _, bad := range []string{
		"bytes=abc-def", // unparseable numbers
		"bytes=5",       // no dash
		"items=0-10",    // unit this server does not implement
		"bytes=-",       // no suffix length
		"bytes=20-10",   // inverted
		"garbage",
	} {
		t.Run(bad, func(t *testing.T) {
			r := serveGET(t, m, "clip.mp4", map[string]string{"range": bad})
			r.wantNoError(t)
			r.wantStatus(t, 206)
			r.wantHeader(t, "Content-Range", "bytes 0-31/100")
			r.wantSentBody(t, testBody[:32])
		})
	}
}

// TestServeStatus304 covers both validators, and the rule that a 304 carries
// neither a body nor a Content-Length — a client that read a length here would
// wait for bytes that never arrive.
func TestServeStatus304(t *testing.T) {
	m, root := bodyMount(t, nil)
	info, err := os.Stat(filepath.Join(root, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	tag := etagFor(info.Size(), info.ModTime().Unix())

	t.Run("if-none-match", func(t *testing.T) {
		r := serveGET(t, m, "clip.mp4", map[string]string{"if-none-match": tag})
		r.wantNoError(t)
		r.wantStatus(t, 304)
		r.wantHeader(t, "ETag", tag)
		r.wantHeader(t, "Accept-Ranges", "bytes")
		r.wantNoHeader(t, "Content-Length")
		r.wantNoHeader(t, "Content-Range")
		r.wantBody(t, "")
		if r.sent != 0 {
			t.Errorf("a 304 reported %d body bytes", r.sent)
		}
	})

	t.Run("if-none-match weak", func(t *testing.T) {
		serveGET(t, m, "clip.mp4", map[string]string{"if-none-match": "W/" + tag}).wantStatus(t, 304)
	})

	t.Run("if-none-match star", func(t *testing.T) {
		serveGET(t, m, "clip.mp4", map[string]string{"if-none-match": "*"}).wantStatus(t, 304)
	})

	t.Run("if-modified-since", func(t *testing.T) {
		later := info.ModTime().Add(time.Hour).Unix()
		r := serveGET(t, m, "clip.mp4", map[string]string{"if-modified-since": httpTime(later)})
		r.wantNoError(t)
		r.wantStatus(t, 304)
		r.wantBody(t, "")
	})

	t.Run("stale date still serves", func(t *testing.T) {
		earlier := info.ModTime().Add(-time.Hour).Unix()
		serveGET(t, m, "clip.mp4", map[string]string{"if-modified-since": httpTime(earlier)}).wantStatus(t, 206)
	})

	t.Run("unparseable date is ignored not honoured", func(t *testing.T) {
		serveGET(t, m, "clip.mp4", map[string]string{"if-modified-since": "last tuesday"}).wantStatus(t, 206)
	})

	t.Run("etag takes precedence over date", func(t *testing.T) {
		// A non-matching ETag alongside a satisfied date must serve: RFC 9110
		// §13.1.3 gives If-None-Match precedence outright, because a tag is
		// exact where a date is only second-accurate.
		r := serveGET(t, m, "clip.mp4", map[string]string{
			"if-none-match":     `"stale"`,
			"if-modified-since": httpTime(info.ModTime().Add(time.Hour).Unix()),
		})
		r.wantStatus(t, 206)
	})
}

// TestServeStatus403 covers the signature failures and an Authorize refusal.
// These are the only 403s the mount emits: everything about a file's
// *existence* is a 404, so a prober cannot map the library by watching which
// refusal it gets.
func TestServeStatus403(t *testing.T) {
	secret := []byte("s3cret")
	now := time.Unix(1700000000, 0)

	signedMount := func(t *testing.T) *mount {
		t.Helper()
		m, _ := bodyMount(t, func(c *Config) {
			c.Secret = secret
			c.Clock = func() time.Time { return now }
		})
		return m
	}

	t.Run("missing signature", func(t *testing.T) {
		r := serveGET(t, signedMount(t), "clip.mp4", nil)
		r.wantStatus(t, 403)
		r.wantBody(t, "Forbidden")
		if !errors.Is(r.err, ErrSignatureRequired) {
			t.Errorf("err = %v, want ErrSignatureRequired", r.err)
		}
	})

	t.Run("invalid signature", func(t *testing.T) {
		r := serveGET(t, signedMount(t), "clip.mp4?exp=9999999999&sig=deadbeef", nil)
		r.wantStatus(t, 403)
		if !errors.Is(r.err, ErrInvalidSignature) {
			t.Errorf("err = %v, want ErrInvalidSignature", r.err)
		}
	})

	t.Run("signature for another file", func(t *testing.T) {
		// One link must not become a key to the whole library.
		r := serveGET(t, signedMount(t), "clip.mp4?"+SignAt(secret, "other.mp4", now.Add(time.Hour)), nil)
		r.wantStatus(t, 403)
		if !errors.Is(r.err, ErrInvalidSignature) {
			t.Errorf("err = %v, want ErrInvalidSignature", r.err)
		}
	})

	t.Run("expired signature", func(t *testing.T) {
		m, _ := bodyMount(t, func(c *Config) {
			c.Secret = secret
			c.Clock = func() time.Time { return now.Add(2 * time.Hour) }
		})
		r := serveGET(t, m, "clip.mp4?"+SignAt(secret, "clip.mp4", now.Add(time.Hour)), nil)
		r.wantStatus(t, 403)
		if !errors.Is(r.err, ErrSignatureExpired) {
			t.Errorf("err = %v, want ErrSignatureExpired", r.err)
		}
	})

	t.Run("valid signature serves", func(t *testing.T) {
		r := serveGET(t, signedMount(t), "clip.mp4?"+SignAt(secret, "clip.mp4", now.Add(time.Hour)),
			map[string]string{"range": "bytes=0-9"})
		r.wantNoError(t)
		r.wantStatus(t, 206)
		r.wantSentBody(t, testBody[:10])
	})

	t.Run("refused before any disk access", func(t *testing.T) {
		// Verifying ahead of stat is what makes an unauthenticated flood cost
		// no I/O. A name that does not exist must still come back 403, not
		// 404, which proves stat never ran.
		serveGET(t, signedMount(t), "absent.mp4", nil).wantStatus(t, 403)
	})

	t.Run("authorize refusal", func(t *testing.T) {
		refused := errors.New("not your video")
		m, _ := bodyMount(t, func(c *Config) {
			c.Authorize = func(ctx *breeze.Context, name string) error { return refused }
		})
		r := serveGET(t, m, "clip.mp4", nil)
		// statusFor maps an unrecognised error to 500; serve rewrites it to
		// 403, because the callback made a decision rather than malfunctioning.
		r.wantStatus(t, 403)
		if !errors.Is(r.err, refused) {
			t.Errorf("err = %v, want the callback's own error", r.err)
		}
	})

	t.Run("authorize may still choose 404", func(t *testing.T) {
		m, _ := bodyMount(t, func(c *Config) {
			c.Authorize = func(ctx *breeze.Context, name string) error { return ErrNotFound }
		})
		// A sentinel keeps its own mapping, so a callback that wants to hide
		// existence can say so.
		serveGET(t, m, "clip.mp4", nil).wantStatus(t, 404)
	})

	t.Run("authorize sees the normalized name and the context", func(t *testing.T) {
		var gotName string
		var gotCtx *breeze.Context
		m, _ := bodyMount(t, func(c *Config) {
			c.Authorize = func(ctx *breeze.Context, name string) error {
				gotName, gotCtx = name, ctx
				return nil
			}
		})
		serveGET(t, m, "clip.mp4", nil).wantStatus(t, 206)
		if gotName != "clip.mp4" {
			t.Errorf("Authorize got name %q, want %q", gotName, "clip.mp4")
		}
		if gotCtx == nil {
			t.Error("Authorize got a nil context, so it cannot read the caller's identity")
		}
	})
}

// TestServeStatus404 walks every way a file can be treated as absent. They must
// be indistinguishable on the wire — that is the whole reason they collapse to
// one code.
func TestServeStatus404(t *testing.T) {
	m, root := testMount(t, map[string]string{
		"clip.mp4":       testBody,
		".hidden.mp4":    "secret",
		"notes.txt":      "text",
		"sub/nested.mp4": "nested",
	}, nil)
	if err := os.MkdirAll(filepath.Join(root, "dir.mp4"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"missing":           "absent.mp4",
		"traversal":         "../secret.mp4",
		"encoded traversal": "%2e%2e%2fsecret.mp4",
		"backslash":         "sub\\nested.mp4",
		"NUL byte":          "clip.mp4\x00.txt",
		"hidden":            ".hidden.mp4",
		"wrong type":        "notes.txt",
		"directory":         "dir.mp4",
		"empty path":        "",
	}
	for desc, target := range cases {
		t.Run(desc, func(t *testing.T) {
			r := serveGET(t, m, target, nil)
			r.wantStatus(t, 404)
			r.wantBody(t, "Not Found")
			r.wantHeader(t, "Content-Type", "text/plain; charset=utf-8")
			r.wantHeader(t, "Cache-Control", "no-store")
			if r.err == nil {
				t.Error("serve reported no error, so nothing reaches OnError or the collector")
			}
		})
	}
}

// TestServeStatus404ForSymlink is the last row of the 404 table. It is separate
// because creating a symlink needs privilege on Windows.
func TestServeStatus404ForSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	m, root := bodyMount(t, nil)
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.mp4")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	r := serveGET(t, m, "link.mp4", nil)
	r.wantStatus(t, 404)
	r.wantBody(t, "Not Found")
}

// TestServeStatus416 covers the one range failure that is a rejection rather
// than an ignore, and the Content-Range that makes it recoverable.
func TestServeStatus416(t *testing.T) {
	m, _ := bodyMount(t, nil)

	for _, spec := range []string{"bytes=100-", "bytes=200-300", "bytes=-0"} {
		t.Run(spec, func(t *testing.T) {
			r := serveGET(t, m, "clip.mp4", map[string]string{"range": spec})
			r.wantStatus(t, 416)
			// Stating the true length is the entire point of the status:
			// without it the client cannot correct its request.
			r.wantHeader(t, "Content-Range", "bytes */100")
			r.wantHeader(t, "Accept-Ranges", "bytes")
			r.wantBody(t, "Range Not Satisfiable")
			if !errors.Is(r.err, ErrRangeNotSatisfiable) {
				t.Errorf("err = %v, want ErrRangeNotSatisfiable", r.err)
			}
			if r.sent != 0 {
				t.Errorf("a 416 reported %d body bytes of the file", r.sent)
			}
		})
	}

	t.Run("empty file cannot satisfy any range", func(t *testing.T) {
		e, _ := testMount(t, map[string]string{"empty.mp4": ""}, nil)
		r := serveGET(t, e, "empty.mp4", map[string]string{"range": "bytes=0-10"})
		r.wantStatus(t, 416)
		r.wantHeader(t, "Content-Range", "bytes */0")
	})
}

// --- Behaviour beyond the status code --------------------------------------

// TestServeHEADMatchesGET pins the reason HEAD is registered at all: a player
// must be able to learn the length and range support without the transfer. If
// the head differed, the player would act on numbers that do not describe the
// GET it makes next.
func TestServeHEADMatchesGET(t *testing.T) {
	m, _ := bodyMount(t, func(c *Config) { c.ChunkSize = 32 })
	headers := map[string]string{"range": "bytes=20-29"}

	g := serveGET(t, m, "clip.mp4", headers)
	g.wantNoError(t)
	h := serveReq(t, m, breeze.Method("HEAD"), "clip.mp4", headers)
	h.wantNoError(t)
	h.wantStatus(t, 206)

	for _, key := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"ETag", "Last-Modified", "Cache-Control", "X-Content-Type-Options",
	} {
		if got, want := h.sink.headerValue(key), g.sink.headerValue(key); got != want {
			t.Errorf("HEAD %s = %q, GET has %q", key, got, want)
		}
	}

	// Content-Length describes what a GET would return, not what HEAD sent.
	h.wantHeader(t, "Content-Length", "10")
	h.wantBody(t, "")
	if h.sent != 0 {
		t.Errorf("HEAD sent %d body bytes", h.sent)
	}
}

// TestServeSuccessHeaders covers the fields that are not about status codes but
// are the difference between a stream a browser will play and one it will not.
func TestServeSuccessHeaders(t *testing.T) {
	m, root := bodyMount(t, func(c *Config) { c.CacheControl = "public, max-age=60" })
	info, err := os.Stat(filepath.Join(root, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}

	r := serveGET(t, m, "clip.mp4", map[string]string{"range": "bytes=0-4"})
	r.wantNoError(t)
	r.wantHeader(t, "Content-Type", "video/mp4")
	r.wantHeader(t, "Accept-Ranges", "bytes")
	r.wantHeader(t, "Cache-Control", "public, max-age=60")
	r.wantHeader(t, "ETag", etagFor(info.Size(), info.ModTime().Unix()))
	r.wantHeader(t, "Last-Modified", httpTime(info.ModTime().Unix()))
	// A mount can be pointed at a directory that also holds .m3u8 manifests,
	// which are text a browser could be talked into rendering.
	r.wantHeader(t, "X-Content-Type-Options", "nosniff")
}

// TestServeIfRange guards a resumed download. A mismatch means the client's
// stored prefix is stale, so continuing at its byte offset would splice new
// bytes onto an old file and corrupt the result.
func TestServeIfRange(t *testing.T) {
	m, root := bodyMount(t, func(c *Config) { c.ChunkSize = 32 })
	info, err := os.Stat(filepath.Join(root, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	tag := etagFor(info.Size(), info.ModTime().Unix())

	t.Run("match honours the range", func(t *testing.T) {
		r := serveGET(t, m, "clip.mp4", map[string]string{"range": "bytes=50-59", "if-range": tag})
		r.wantNoError(t)
		r.wantStatus(t, 206)
		r.wantHeader(t, "Content-Range", "bytes 50-59/100")
		r.wantSentBody(t, testBody[50:60])
	})

	t.Run("mismatch discards the range", func(t *testing.T) {
		r := serveGET(t, m, "clip.mp4", map[string]string{"range": "bytes=50-59", "if-range": `"stale-1"`})
		r.wantNoError(t)
		r.wantStatus(t, 206)
		// Back to the first chunk: the client must restart, not resume.
		r.wantHeader(t, "Content-Range", "bytes 0-31/100")
		r.wantSentBody(t, testBody[:32])
	})

	t.Run("without a range it is inert", func(t *testing.T) {
		// If-Range only qualifies a Range; alone it must not change anything.
		r := serveGET(t, m, "clip.mp4", map[string]string{"if-range": `"stale-1"`})
		r.wantStatus(t, 206)
		r.wantHeader(t, "Content-Range", "bytes 0-31/100")
	})
}

// TestServeMaxChunkCap covers the ceiling on a range the client did ask for.
// An open-ended "bytes=0-" is a player's opening request; honouring it
// literally would pin an entire movie in one response.
func TestServeMaxChunkCap(t *testing.T) {
	m, _ := bodyMount(t, func(c *Config) { c.MaxChunkSize = 16 })

	r := serveGET(t, m, "clip.mp4", map[string]string{"range": "bytes=0-"})
	r.wantNoError(t)
	r.wantStatus(t, 206)
	// Content-Range states exactly what was sent, which is what makes
	// returning less than was asked for legal.
	r.wantHeader(t, "Content-Range", "bytes 0-15/100")
	r.wantHeader(t, "Content-Length", "16")
	r.wantSentBody(t, testBody[:16])
}

// TestServeCORS covers the headers that let browser JavaScript read a stream,
// and the Vary that keeps a shared cache from replaying one origin's answer to
// another.
func TestServeCORS(t *testing.T) {
	const allowed = "https://app.example"

	t.Run("allowed origin", func(t *testing.T) {
		m, _ := bodyMount(t, func(c *Config) { c.AllowedOrigins = []string{allowed} })
		r := serveGET(t, m, "clip.mp4", map[string]string{"origin": allowed})
		r.wantHeader(t, "Access-Control-Allow-Origin", allowed)
		r.wantHeader(t, "Vary", "Origin")
		// Without this, JavaScript can read the body but not Content-Range, so
		// a player cannot discover the total length.
		if got := r.sink.headerValue("Access-Control-Expose-Headers"); !strings.Contains(got, "Content-Range") {
			t.Errorf("Access-Control-Expose-Headers = %q, must expose Content-Range", got)
		}
	})

	t.Run("refused origin still varies", func(t *testing.T) {
		m, _ := bodyMount(t, func(c *Config) { c.AllowedOrigins = []string{allowed} })
		r := serveGET(t, m, "clip.mp4", map[string]string{"origin": "https://evil.example"})
		r.wantNoHeader(t, "Access-Control-Allow-Origin")
		// The refusal is itself origin-dependent, so it must not be cached
		// across origins either.
		r.wantHeader(t, "Vary", "Origin")
	})

	t.Run("wildcard echoes star", func(t *testing.T) {
		m, _ := bodyMount(t, func(c *Config) { c.AllowedOrigins = []string{"*"} })
		r := serveGET(t, m, "clip.mp4", map[string]string{"origin": allowed})
		r.wantHeader(t, "Access-Control-Allow-Origin", "*")
	})

	t.Run("error responses carry it too", func(t *testing.T) {
		// A 404 that omitted CORS would surface in the browser as an opaque
		// network error rather than a 404.
		m, _ := bodyMount(t, func(c *Config) { c.AllowedOrigins = []string{allowed} })
		r := serveGET(t, m, "absent.mp4", map[string]string{"origin": allowed})
		r.wantStatus(t, 404)
		r.wantHeader(t, "Access-Control-Allow-Origin", allowed)
		r.wantHeader(t, "Vary", "Origin")
	})

	t.Run("unconfigured emits nothing", func(t *testing.T) {
		m, _ := bodyMount(t, nil)
		r := serveGET(t, m, "clip.mp4", map[string]string{"origin": allowed})
		r.wantNoHeader(t, "Access-Control-Allow-Origin")
		r.wantNoHeader(t, "Vary")
	})
}

// TestServeOpaqueToken covers the mode where the URL carries a sealed token
// instead of a name, so the path reveals nothing about the library.
func TestServeOpaqueToken(t *testing.T) {
	secret := []byte("s3cret")
	now := time.Unix(1700000000, 0)
	m, _ := bodyMount(t, func(c *Config) {
		c.Opaque = true
		c.Secret = secret
		c.Clock = func() time.Time { return now }
	})

	tok, err := TokenAt(secret, "clip.mp4", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("TokenAt: %v", err)
	}

	t.Run("valid token serves the sealed name", func(t *testing.T) {
		r := serveGET(t, m, tok, map[string]string{"range": "bytes=0-9"})
		r.wantNoError(t)
		r.wantStatus(t, 206)
		r.wantSentBody(t, testBody[:10])
		// The name reported to telemetry is the real file, not the token, or
		// the dashboard would group every request under its own URL.
		if r.name != "clip.mp4" {
			t.Errorf("name = %q, want the sealed name", r.name)
		}
	})

	t.Run("plain name is not a token", func(t *testing.T) {
		// If the real filename still worked, the mode would be decorative.
		r := serveGET(t, m, "clip.mp4", nil)
		if r.status == 200 || r.status == 206 {
			t.Fatalf("opaque mount served an unsealed name with %d", r.status)
		}
	})

	t.Run("garbage token", func(t *testing.T) {
		r := serveGET(t, m, "not-a-token", nil)
		r.wantStatus(t, 403)
		if !errors.Is(r.err, ErrInvalidSignature) {
			t.Errorf("err = %v, want ErrInvalidSignature", r.err)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		late, _ := bodyMount(t, func(c *Config) {
			c.Opaque = true
			c.Secret = secret
			c.Clock = func() time.Time { return now.Add(2 * time.Hour) }
		})
		r := serveGET(t, late, tok, nil)
		r.wantStatus(t, 403)
		if !errors.Is(r.err, ErrSignatureExpired) {
			t.Errorf("err = %v, want ErrSignatureExpired", r.err)
		}
	})
}

// TestServeErrorsRevealNothing is the wire-level half of the 404-not-403 rule.
// The internal error carries the detail an operator needs; the response must
// carry none of it. A body saying "escapes root" confirms the traversal was
// understood, and the root path leaks the deployment layout.
func TestServeErrorsRevealNothing(t *testing.T) {
	m, root := bodyMount(t, nil)

	for _, target := range []string{"../../etc/passwd.mp4", "absent.mp4", "%2e%2e%2fsecret.mp4"} {
		t.Run(target, func(t *testing.T) {
			r := serveGET(t, m, target, nil)
			wire := r.sink.all()

			for _, leak := range []string{
				root, filepath.ToSlash(root),
				"dot-dot", "escapes", "outside root", "no such file", "undecodable",
			} {
				if strings.Contains(wire, leak) {
					t.Errorf("response leaks %q:\n%s", leak, wire)
				}
			}

			// The detail must still exist — for the log, not the client.
			if r.err == nil {
				t.Fatal("no internal error, so OnError and the collector learn nothing")
			}
			if r.err.Error() == errText(r.status) {
				t.Errorf("internal error is only the reason phrase (%q); the detail was lost", r.err)
			}
		})
	}
}

// TestServeReportsShortSendOnDisconnect covers what report() consumes. A viewer
// closing the tab aborts the write mid-body: the head is already gone so the
// status stands, and what matters is that serve hands back the error and the
// true byte count, which is how the outcome is classified as cancelled rather
// than as a server failure.
func TestServeReportsShortSendOnDisconnect(t *testing.T) {
	m, _ := bodyMount(t, func(c *Config) { c.ChunkSize = 16 })

	// Write 1 is the head, write 2 the first body chunk.
	out := &bufSink{failAt: 2}
	r := serveReqWith(t, m, breeze.GET, "clip.mp4", map[string]string{"range": "bytes=0-63"}, out)

	r.wantStatus(t, 206)
	if r.err == nil {
		t.Fatal("serve reported success after the peer went away")
	}
	if !isPeerGone(r.err) {
		t.Errorf("err = %v, which report() would count as a server failure", r.err)
	}
	if r.sent != 0 {
		t.Errorf("sent = %d, want 0: no chunk was accepted", r.sent)
	}
}

// TestServeSizeConstantsMatchFixture keeps the fixture honest, since every
// Content-Range assertion above hardcodes 100.
func TestServeSizeConstantsMatchFixture(t *testing.T) {
	if len(testBody) != testSize {
		t.Fatalf("testBody is %d bytes but testSize says %d", len(testBody), testSize)
	}
}
