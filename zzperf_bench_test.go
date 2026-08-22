package breeze

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
)

// zzperf_bench_test.go — microbenchmarks for the request path.
//
// These exist to make the throughput work measurable rather than assumed. Each
// benchmark isolates one stage that was changed, and pairs the old approach
// against the new one where both are still reachable, so a regression shows up
// as a number instead of an argument.
//
// gnet I/O is deliberately absent: on Windows gnet falls back to a
// goroutine-per-connection model, so an end-to-end local run measures the
// wrong thing. What is left — parse, route, serialize, dispatch — is where the
// per-request cost actually lives, and it is platform-independent.
//
// Run:
//
//	go test -run XXX -bench . -benchmem
//
// The interesting comparisons:
//
//	ParsePublic     vs ParsePooled            — request + header-map pooling
//	ParsePooled     vs ParsePooledZeroCopy    — the owned header-block copy
//	LookupStaticMap vs LookupOrderedScan      — the exact-path map
//	SerializeAlloc  vs SerializePooled        — AppendTo + wireBufPool
//	PoolSubmit      vs InlineCall             — what inline execution removes
//	PipelineDispatch vs PipelineInline vs PipelineInlineZeroCopy
//	                                          — the whole request path, old to new

var rawGET = []byte("GET /users/42 HTTP/1.1\r\nHost: localhost:3000\r\nUser-Agent: bombardier\r\nAccept: */*\r\nConnection: keep-alive\r\n\r\n")

// rawGETStatic targets a route that lives in the method bucket's exact-path
// map, so the lookup never splits the path.
var rawGETStatic = []byte("GET /users HTTP/1.1\r\nHost: localhost:3000\r\nUser-Agent: bombardier\r\nAccept: */*\r\nConnection: keep-alive\r\n\r\n")

// rawGETScanned targets a static route that a previously-registered :param
// route also matches, so it cannot live in the exact-path map — the ordered scan
// would reach the :param route first, and the map has to agree with the scan.
// It is therefore the honest baseline for what the map buys.
var rawGETScanned = []byte("GET /users/me HTTP/1.1\r\nHost: localhost:3000\r\nUser-Agent: bombardier\r\nAccept: */*\r\nConnection: keep-alive\r\n\r\n")

// perfRouter mirrors a small real routing table. Registration order matters:
// "/users" precedes "/users/:id" and nothing shadows it, so it is map-eligible;
// "/users/me" follows "/users/:id", which matches the same path, so it is not.
//
// "/health" IS map-eligible even though it is registered after a :param route —
// map eligibility is per-path, not per-bucket, and no dynamic route here has a
// single segment. That is the whole point of the eligibility test in
// indexRoute; a benchmark that assumed otherwise would be timing the map while
// claiming to time the scan.
func perfRouter() *Router {
	r := NewRouter()
	r.autoServeRoot = false
	r.Use(func(c *Context) { c.Next() })
	r.Handle(GET, "/users", func(c *Context) { c.JSON(map[string]string{"a": "b"}) })
	r.Handle(GET, "/users/:id", func(c *Context) {
		c.JSON(map[string]string{"id": c.GetParam("id"), "name": "Alice"})
	})
	r.Handle(POST, "/users", func(c *Context) {})
	r.Handle(GET, "/users/me", func(c *Context) {})
	r.Handle(GET, "/health", func(c *Context) {})
	return r
}

// mustParse parses raw once for benchmarks that measure a later stage. It
// returns a request that is never released, so the parse cost stays out of the
// timed loop.
func mustParse(tb testing.TB, raw []byte) *HTTPRequest {
	tb.Helper()
	req, _, err := ParseHTTPRequest(raw)
	if err != nil || req == nil {
		tb.Fatalf("parse failed: %v", err)
	}
	return req
}

// rawGETBrowser is what a real browser sends: eleven headers, every one of
// them capitalised. Header-key handling used to allocate twice per key, so
// this request paid ~22 allocations before it reached the router. It is here
// to keep that from coming back.
var rawGETBrowser = []byte("GET /users/42 HTTP/1.1\r\n" +
	"Host: localhost:3000\r\n" +
	"Connection: keep-alive\r\n" +
	"Cache-Control: max-age=0\r\n" +
	"Upgrade-Insecure-Requests: 1\r\n" +
	"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36\r\n" +
	"Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\n" +
	"Accept-Encoding: gzip, deflate, br\r\n" +
	"Accept-Language: en-US,en;q=0.9\r\n" +
	"Sec-Fetch-Dest: document\r\n" +
	"Sec-Fetch-Mode: navigate\r\n" +
	"Cookie: session=abc123; theme=dark\r\n" +
	"\r\n")

// ── Parsing ──────────────────────────────────────────────────────────────────

// rawCopy returns a private copy of raw, for benchmarks that parse zero-copy.
//
// A zero-copy parse lowercases header keys inside the buffer it was handed. A
// benchmark that fed it a shared fixture would leave rawGET permanently
// lowercased, and every benchmark running after it — Go runs them in source
// order — would then be measuring a parse whose keys need no recasing, which is
// not a request any client sends. One copy per benchmark, made outside the timed
// loop, keeps the fixtures independent at zero per-iteration cost.
//
// Within a single zero-copy benchmark the copy is still lowercased after the
// first iteration, so iterations 2..N skip the byte stores that recasing would
// do. Avoiding that would mean re-copying the bytes every iteration, which costs
// about what the owned header copy costs — precisely the difference these
// benchmarks exist to measure. The residual bias is roughly five byte stores per
// iteration for a four-header GET, against a parse an order of magnitude larger.
func rawCopy(raw []byte) []byte {
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

// BenchmarkZZParsePublic measures the exported parser, which allocates a fresh
// request and header map every call. External callers keep this behaviour.
func BenchmarkZZParsePublic(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _, err := ParseHTTPRequest(rawGET)
		if err != nil || req == nil {
			b.Fatal("parse failed")
		}
	}
}

// BenchmarkZZParsePooled measures the server's parser in its default
// configuration: the request struct and its header map come from requestPool
// and the keys are lowercased in place, so the only remaining allocation is the
// owned header-block copy.
func BenchmarkZZParsePooled(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _, err := parsePooledRequest(rawGET, true)
		if err != nil || req == nil {
			b.Fatal("parse failed")
		}
		releaseRequest(req)
	}
}

// BenchmarkZZParsePooledZeroCopy is the same parse with SetZeroCopyHeaders on:
// the header block is read in place instead of copied, so the whole parse
// allocates nothing. The gap against ParsePooled is the last allocation the
// request path had left.
func BenchmarkZZParsePooledZeroCopy(b *testing.B) {
	raw := rawCopy(rawGET)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _, err := parsePooledRequest(raw, false)
		if err != nil || req == nil {
			b.Fatal("parse failed")
		}
		if req.owned != nil {
			b.Fatal("zero-copy parse still copied the header block")
		}
		releaseRequest(req)
	}
}

// BenchmarkZZPromote measures what a blocking route pays under zero-copy: a
// zero-copy parse followed by the re-parse into owned memory that lets the
// request leave the event-loop goroutine. It should land close to ParsePooled
// plus ParsePooledZeroCopy — i.e. the promotion is a second parse and nothing
// more — and it is only ever paid by routes that are about to block on I/O.
func BenchmarkZZPromote(b *testing.B) {
	raw := rawCopy(rawGET)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _, err := parsePooledRequest(raw, false)
		if err != nil || req == nil {
			b.Fatal("parse failed")
		}
		if err := promoteRequest(req, raw); err != nil {
			b.Fatalf("promote failed: %v", err)
		}
		if req.owned == nil {
			b.Fatal("promotion did not take ownership")
		}
		releaseRequest(req)
	}
}

// BenchmarkZZParseBrowserHeaders is the regression guard for header-key
// handling. Allocations here should stay at one — the owned copy — and must
// not scale with the header count.
func BenchmarkZZParseBrowserHeaders(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _, err := parsePooledRequest(rawGETBrowser, true)
		if err != nil || req == nil {
			b.Fatal("parse failed")
		}
		if req.Header["user-agent"] == "" {
			b.Fatal("header keys not lowercased")
		}
		releaseRequest(req)
	}
}

// ── Routing ──────────────────────────────────────────────────────────────────

// BenchmarkZZLookupStaticMap resolves a route through the bucket's exact-path
// map: one hash probe, no path split, no scan.
func BenchmarkZZLookupStaticMap(b *testing.B) {
	r := perfRouter()
	req := mustParse(b, rawGETStatic)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt, params, _ := r.lookup(req)
		if rt == nil {
			b.Fatal("no route")
		}
		if params != nil {
			releaseParams(params)
		}
	}
}

// BenchmarkZZLookupOrderedScan resolves a static route that the map cannot
// hold, so it pays the path split and the ordered walk. This is what every
// lookup used to cost.
func BenchmarkZZLookupOrderedScan(b *testing.B) {
	r := perfRouter()
	req := mustParse(b, rawGETScanned)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt, params, _ := r.lookup(req)
		if rt == nil {
			b.Fatal("no route")
		}
		if params != nil {
			releaseParams(params)
		}
	}
}

// BenchmarkZZLookupParam resolves a :param route, which allocates nothing but
// takes a pooled params map.
func BenchmarkZZLookupParam(b *testing.B) {
	r := perfRouter()
	req := mustParse(b, rawGET)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt, params, _ := r.lookup(req)
		if rt == nil {
			b.Fatal("no route")
		}
		if params != nil {
			releaseParams(params)
		}
	}
}

// ── Serialization ────────────────────────────────────────────────────────────

func benchResponse() *HTTPResponse {
	return &HTTPResponse{
		Status:        200,
		Headers:       hdrsJSON,
		headersShared: true,
		rawHeaders:    rawJSON,
		Body:          []byte(`{"id":"42","name":"Alice"}`),
	}
}

// BenchmarkZZSerializeAlloc is the AsyncWrite path: Bytes allocates a slice
// because gnet keeps it until the poller drains the write.
func BenchmarkZZSerializeAlloc(b *testing.B) {
	res := benchResponse()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if len(res.Bytes()) == 0 {
			b.Fatal("empty")
		}
	}
}

// BenchmarkZZSerializePooled is the inline path: AppendTo writes into a buffer
// from wireBufPool, which Conn.Write has finished with by the time it returns.
func BenchmarkZZSerializePooled(b *testing.B) {
	res := benchResponse()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bp := acquireWireBuf()
		*bp = res.AppendTo(*bp)
		if len(*bp) == 0 {
			b.Fatal("empty")
		}
		releaseWireBuf(bp)
	}
}

// ── Full request path ────────────────────────────────────────────────────────

// BenchmarkZZPipelineInline runs everything OnTraffic does for a non-blocking
// route except the gnet read and write syscalls: pooled parse, route, chain,
// handler, and serialization into a pooled buffer.
func BenchmarkZZPipelineInline(b *testing.B) {
	benchPipelineInline(b, true)
}

// BenchmarkZZPipelineInlineZeroCopy is the same path with SetZeroCopyHeaders on.
// Every allocation the framework itself makes is gone; what remains is whatever
// the handler allocates — here the map and the JSON it marshals.
func BenchmarkZZPipelineInlineZeroCopy(b *testing.B) {
	benchPipelineInline(b, false)
}

func benchPipelineInline(b *testing.B, ownHeaders bool) {
	r := perfRouter()
	// Private copy: a zero-copy parse writes lowercased keys back into the
	// buffer it is given. See rawCopy.
	raw := rawCopy(rawGET)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _, err := parsePooledRequest(raw, ownHeaders)
		if err != nil || req == nil {
			b.Fatal("parse failed")
		}
		chain, params, _ := r.findDispatch(req)
		if chain == nil {
			b.Fatal("no route")
		}
		ctx := acquireContext()
		ctx.Req = req
		ctx.reqPooled = true
		ctx.params = params
		ctx.middlewares = chain
		ctx.index = -1
		ctx.Next()
		if ctx.Res != nil {
			bp := acquireWireBuf()
			*bp = ctx.Res.AppendTo(*bp)
			releaseWireBuf(bp)
		}
		releaseContext(ctx)
	}
}

// BenchmarkZZPipelineDispatch is the same request answered the old way: the
// public allocating parser, a per-event whole-request copy, and a freshly
// allocated wire slice for AsyncWrite. The gap against PipelineInline is the
// per-request work that inline execution removed.
func BenchmarkZZPipelineDispatch(b *testing.B) {
	r := perfRouter()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := append([]byte(nil), rawGET...) // the per-event copy that is now gone
		req, _, _ := ParseHTTPRequest(buf)
		chain, params := r.findChain(req)
		if chain == nil {
			b.Fatal("no route")
		}
		ctx := acquireContext()
		ctx.Req = req
		ctx.params = params
		ctx.middlewares = chain
		ctx.index = -1
		ctx.Next()
		if ctx.Res != nil {
			_ = ctx.Res.Bytes()
		}
		releaseContext(ctx)
	}
}

// ── Handler cost ─────────────────────────────────────────────────────────────
//
// With zero-copy headers on, the framework's own per-request allocation count is
// zero: parse, route and serialize are all allocation-free (see the benchmarks
// above). Every allocation PipelineInlineZeroCopy still reports therefore comes
// from the handler, and on a JSON route that means ctx.JSON.
//
// These three benchmarks separate the two costs ctx.JSON pays — building the
// value and reflecting over it — so it is clear which one to attack.

// BenchmarkZZJSONMapHandler is the shape cmd/main.go's handlers use:
// json.Marshal over a freshly built map. The map allocates, its keys hash, and
// encoding/json walks it by reflection, sorting the keys before writing them.
func BenchmarkZZJSONMapHandler(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := json.Marshal(map[string]string{"id": "42", "name": "Alice"})
		if err != nil || len(out) == 0 {
			b.Fatal("marshal failed")
		}
	}
}

// BenchmarkZZJSONStructHandler marshals the same payload from a struct.
// encoding/json caches a per-type encoder, so a struct skips both the map
// allocation and the key sort while staying ordinary idiomatic Go.
func BenchmarkZZJSONStructHandler(b *testing.B) {
	type user struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := json.Marshal(user{ID: "42", Name: "Alice"})
		if err != nil || len(out) == 0 {
			b.Fatal("marshal failed")
		}
	}
}

// BenchmarkZZJSONAppendHandler builds the same bytes by hand into a pooled
// buffer — no reflection, no allocation. It is the floor the two above are
// measured against, and what a hot route can drop to when it matters.
func BenchmarkZZJSONAppendHandler(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bp := acquireWireBuf()
		buf := append(*bp, `{"id":"`...)
		buf = append(buf, "42"...)
		buf = append(buf, `","name":"`...)
		buf = append(buf, "Alice"...)
		buf = append(buf, `"}`...)
		if len(buf) == 0 {
			b.Fatal("empty")
		}
		*bp = buf
		releaseWireBuf(bp)
	}
}

// ── Dispatch overhead ────────────────────────────────────────────────────────

// BenchmarkZZPoolSubmit measures the worker-pool round trip every request used
// to pay: a channel send guarded by one mutex that all event loops contend
// for, plus a goroutine handoff. %spawned reports how often the queue was full
// and OverflowSpawn degenerated into goroutine-per-request.
func BenchmarkZZPoolSubmit(b *testing.B) {
	p := NewEventLoopWorkerPool(runtime.NumCPU())
	defer p.Shutdown(context.Background()) // else every run leaks NumCPU workers

	var wg sync.WaitGroup
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			wg.Add(1)
			// SubmitErr, not Submit: this pool is OverflowSpawn so a task is
			// never dropped today, but Submit swallows rejection. If the
			// policy ever changes, a dropped task means a Done that never
			// runs and a benchmark that hangs instead of failing.
			if err := p.SubmitErr(func() { wg.Done() }); err != nil {
				wg.Done()
			}
		}
	})
	wg.Wait()
	b.StopTimer()
	m := p.Metrics()
	b.ReportMetric(float64(m.Spawned)/float64(b.N)*100, "%spawned")
}

// BenchmarkZZInlineCall is the floor PoolSubmit is measured against: the same
// work, called directly on the calling goroutine.
func BenchmarkZZInlineCall(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			func() {}()
		}
	})
}
