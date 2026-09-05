package video

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	breeze "github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/observability"
)

// Default tuning values. They are exported so callers can reason about
// what they are overriding rather than guessing.
const (
	// DefaultChunkSize is how much of a file is read and handed to the
	// connection per write. 256 KiB is large enough that syscall overhead
	// is amortised on multi-megabyte reads, and small enough that a
	// thousand concurrent viewers cost ~256 MiB of transient buffers
	// rather than the whole file each.
	DefaultChunkSize = 256 << 10

	// DefaultMaxChunkSize caps how much a single response body may carry
	// when the client asks for an open-ended range ("bytes=0-"). Players
	// routinely send that on the first request; answering with the entire
	// file defeats seeking and pins one file per connection in memory.
	// A server is explicitly allowed to return fewer bytes than asked
	// for, so long as Content-Range describes what it actually sent
	// (RFC 9110 §14.4).
	DefaultMaxChunkSize = 4 << 20

	// DefaultCacheControl is conservative: media is immutable in
	// practice but the URL is not content-addressed, so a day of caching
	// with revalidation is the safe default.
	DefaultCacheControl = "public, max-age=86400"
)

// defaultExtensions is the allow-list applied when Config.Extensions is
// empty. It covers progressive download formats plus the HLS and DASH
// manifest/segment types, because a video mount that cannot serve
// .m3u8 and .ts is useless for adaptive streaming.
var defaultExtensions = []string{
	".mp4", ".m4v", ".webm", ".ogv", ".ogg", ".mov", ".mkv", ".avi",
	".m3u8", ".ts", ".mpd", ".m4s",
}

// mimeTypes maps extension to Content-Type. This package does not consult
// the OS MIME registry: mime.TypeByExtension reads /etc/mime.types and the
// Windows registry, so identical code would serve different content types
// on different machines, and a wrong type on a video makes a browser
// download the file instead of playing it. An explicit table is
// deterministic.
var mimeTypes = map[string]string{
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".webm": "video/webm",
	".ogv":  "video/ogg",
	".ogg":  "video/ogg",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".avi":  "video/x-msvideo",
	".m3u8": "application/vnd.apple.mpegurl",
	".ts":   "video/mp2t",
	".mpd":  "application/dash+xml",
	".m4s":  "video/iso.segment",
}

// contentTypeFor returns the media type for a path, or
// application/octet-stream when the extension is unknown. An unknown type
// is never guessed from the file's bytes: sniffing a user-supplied file
// and echoing the result is how an "video" mount ends up serving
// text/html and hosting a stored XSS.
func contentTypeFor(name string) string {
	if ct, ok := mimeTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// Config configures a video mount. The zero value is not usable — Root is
// required — but every other field has a working default, so the common
// case is:
//
//	video.Mount(router, video.Config{Root: "./media"})
type Config struct {
	// Root is the only directory files are served from. It is resolved
	// to an absolute path with symlinks evaluated at mount time, and
	// every request is proven to stay inside it.
	Root string

	// Prefix is the URL prefix to mount under. Defaults to "/videos".
	// The catch-all route registered is Prefix + "/*filepath".
	Prefix string

	// Extensions is the allow-list of servable extensions, matched
	// case-insensitively and including the leading dot. Empty means
	// [defaultExtensions]. An allow-list rather than a deny-list is
	// deliberate: a new dangerous extension appearing in the world must
	// not silently become servable.
	Extensions []string

	// AllowHidden permits dotfiles and dot-directories. Off by default,
	// which keeps .env, .git and .htpasswd unreachable even if someone
	// drops them into the media root.
	AllowHidden bool

	// FollowSymlinks allows a symlink inside Root to be served. Even
	// when enabled the link's target must itself resolve inside Root, so
	// a link to /etc/passwd is still refused. Off by default.
	FollowSymlinks bool

	// ChunkSize is the per-write read size in bytes. Defaults to
	// [DefaultChunkSize].
	ChunkSize int

	// MaxChunkSize caps the bytes returned for one request when the
	// client sends an open-ended range. Defaults to
	// [DefaultMaxChunkSize]; a negative value disables the cap and
	// serves whatever was asked for.
	//
	// This is a per-response ceiling, not a per-write size: a 4 MiB reply
	// is delivered as ChunkSize writes. Seeing a single request report
	// [DefaultMaxChunkSize] bytes while ChunkSize is 128 KiB is therefore
	// expected — it is 32 writes, not one.
	MaxChunkSize int64

	// Opaque replaces the file name in the URL with an encrypted token,
	// so a link discloses nothing about what it points at or how the
	// library is arranged. Requires Secret.
	//
	// Signed URLs authenticate a link but still name the file in the
	// path, which leaks into referrer headers, proxy logs and bookmarks,
	// and invites guessing at neighbours. With Opaque set, the mount
	// serves only [Token] paths and rejects plain names, so the file is
	// unreachable without a token this server issued:
	//
	//	/videos/<token>            rather than
	//	/videos/dir/file.mp4?exp=…&sig=…
	//
	// The token authenticates and carries its own expiry, so exp/sig is
	// neither required nor consulted in this mode.
	Opaque bool

	// CacheControl is sent on successful responses. Defaults to
	// [DefaultCacheControl]. Set to "-" to omit the header entirely.
	CacheControl string

	// AllowedOrigins enables CORS for exactly these origins. A single
	// "*" entry allows any origin. Empty disables CORS headers, because
	// same-origin playback needs none and emitting them unasked widens
	// the attack surface for no benefit.
	AllowedOrigins []string

	// Authorize, when non-nil, runs before any filesystem access. name
	// is the cleaned request-relative path. Returning a non-nil error
	// aborts the request; sentinel errors from this package keep their
	// status, anything else becomes 403.
	Authorize func(ctx *breeze.Context, name string) error

	// Secret enables signed URLs. When set, a request must carry valid
	// exp and sig query parameters — see [Sign]. Unset means URLs are
	// unsigned and anyone who knows the path can fetch it.
	Secret []byte

	// Clock is the time source used for signature expiry. Defaults to
	// time.Now. Tests inject a fixed clock.
	Clock func() time.Time

	// Bus receives the lifecycle events in [events]. Defaults to
	// [events.Default]. Set DisableEvents to opt out.
	Bus *events.Bus

	// Collector receives observability signals. Defaults to
	// [observability.Default]. Set DisableObservability to opt out.
	Collector *observability.Collector

	// DisableEvents stops event publication. It exists because a nil Bus
	// must mean "use the default", not "publish nothing".
	DisableEvents bool

	// DisableObservability stops signal publication, for the same
	// reason.
	DisableObservability bool

	// OnError receives every request that failed, with the internal
	// error before it was flattened into a status code. Defaults to nil.
	OnError func(ctx *breeze.Context, name string, err error)
}

// mount is the validated, immutable form of a Config. Splitting it from
// Config means request handling never re-derives defaults, re-cleans the
// root, or re-parses the extension list: all of that happens once.
type mount struct {
	root       string
	prefix     string
	exts       map[string]struct{}
	allowHid   bool
	followLink bool
	chunk      int
	maxChunk   int64
	cache      string
	origins    map[string]struct{}
	anyOrigin  bool
	authorize  func(*breeze.Context, string) error
	secret     []byte
	opaque     bool
	clock      func() time.Time

	bus     *events.Bus
	col     *observability.Collector
	onError func(*breeze.Context, string, error)

	// bufs supplies the read buffers used while streaming. It is owned by
	// the mount because its buffer size is the mount's chunk size.
	bufs *sync.Pool

	// Counters for the diagnostic probe.
	//
	// Unconditional atomics, not diag.Counter's gated ones. The unit here is one
	// video response, which stats a file, opens it, and copies at least a chunk
	// out of it — tens of kilobytes of file I/O minimum. Three atomic adds
	// against that is not a measurable cost, and gating them would mean a mount
	// that cannot say how much bandwidth it served unless someone had thought to
	// enable counting first.
	//
	// They live on the mount rather than in a package global because a process
	// may serve several mounts with different roots, and totalling them would
	// make "which library is consuming the bandwidth" unanswerable.
	served       atomic.Uint64
	partial      atomic.Uint64
	failedReqs   atomic.Uint64
	disconnects  atomic.Uint64
	bytesSent    atomic.Uint64
	lastServedNs atomic.Int64
}

// newMount validates cfg and applies defaults.
//
// Everything that can fail is checked here, at startup, where the error
// reaches a developer. A misconfigured root must not turn into a 500 per
// request discovered in production.
func newMount(cfg Config) (*mount, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, fmt.Errorf("%w: Root is required", ErrInvalidConfig)
	}

	// Resolve the root once: absolute, with symlinks evaluated. If the
	// root itself is reached through a link, containment checks must
	// compare against the real path or every request would look like an
	// escape.
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolving Root: %v", ErrInvalidConfig, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: Root %q is not accessible: %v", ErrInvalidConfig, cfg.Root, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("%w: Root %q: %v", ErrInvalidConfig, cfg.Root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: Root %q is not a directory", ErrInvalidConfig, cfg.Root)
	}

	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "/videos"
	}
	if !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("%w: Prefix %q must start with /", ErrInvalidConfig, prefix)
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return nil, fmt.Errorf("%w: Prefix must not be %q", ErrInvalidConfig, "/")
	}

	list := cfg.Extensions
	if len(list) == 0 {
		list = defaultExtensions
	}
	exts := make(map[string]struct{}, len(list))
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		exts[e] = struct{}{}
	}
	if len(exts) == 0 {
		return nil, fmt.Errorf("%w: Extensions contained no usable entries", ErrInvalidConfig)
	}

	chunk := cfg.ChunkSize
	if chunk <= 0 {
		chunk = DefaultChunkSize
	}
	maxChunk := cfg.MaxChunkSize
	switch {
	case maxChunk < 0:
		maxChunk = 0 // explicit opt-out of the cap
	case maxChunk == 0:
		maxChunk = DefaultMaxChunkSize
	}

	// Opaque mode without a secret cannot work: there is no key to seal
	// tokens with, and every request would arrive as an unopenable
	// string. Failing here beats a mount that 403s every request.
	if cfg.Opaque && len(cfg.Secret) == 0 {
		return nil, fmt.Errorf("%w: Opaque requires Secret", ErrInvalidConfig)
	}

	cache := cfg.CacheControl
	if cache == "" {
		cache = DefaultCacheControl
	}

	if cache == "-" {
		cache = ""
	}

	origins := make(map[string]struct{}, len(cfg.AllowedOrigins))
	anyOrigin := false
	for _, o := range cfg.AllowedOrigins {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "*" {
			anyOrigin = true
			continue
		}
		origins[o] = struct{}{}
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	bus := cfg.Bus
	if bus == nil && !cfg.DisableEvents {
		bus = events.Default
	}
	if cfg.DisableEvents {
		bus = nil
	}

	col := cfg.Collector
	if col == nil && !cfg.DisableObservability {
		col = observability.Default()
	}
	if cfg.DisableObservability {
		col = nil
	}

	m := &mount{
		root:       real,
		prefix:     prefix,
		exts:       exts,
		allowHid:   cfg.AllowHidden,
		followLink: cfg.FollowSymlinks,
		chunk:      chunk,
		maxChunk:   maxChunk,
		cache:      cache,
		origins:    origins,
		anyOrigin:  anyOrigin,
		authorize:  cfg.Authorize,
		secret:     cfg.Secret,
		opaque:     cfg.Opaque,
		clock:      clock,

		bus:     bus,
		col:     col,
		onError: cfg.OnError,
		bufs:    newBufferPool(chunk),
	}
	// One registry append per mount, at construction. The request path is
	// untouched; see diag.go.
	m.registerDiagnostics()
	return m, nil
}

// allowsExt reports whether name's extension is servable.
func (m *mount) allowsExt(name string) bool {
	_, ok := m.exts[strings.ToLower(filepath.Ext(name))]
	return ok
}
