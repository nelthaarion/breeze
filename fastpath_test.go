package breeze

import (
	"bytes"
	"math"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nelthaarion/gnetplus/v2"
	"github.com/nelthaarion/go-json-plus"
)

func TestJSONFastParity(t *testing.T) {
	type sampleStruct struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}

	// Build a string exercising every ASCII control byte, quotes, backslashes,
	// HTML characters, U+2028/U+2029, valid multi-byte UTF-8, and invalid UTF-8.
	var tricky strings.Builder
	for b := 0; b < 0x20; b++ {
		tricky.WriteByte(byte(b))
	}
	tricky.WriteString(`"\\<>&`)
	tricky.WriteRune('\u2028')
	tricky.WriteRune('\u2029')
	tricky.WriteString("hello 世界 سلام 🚀 ")
	tricky.Write([]byte{0x80, 0xff, 0xc3, 0x28, 0xe2, 0x82, 0x28})
	trickyStr := tricky.String()

	manyStr := make(map[string]string, 20)
	for i := 0; i < 20; i++ {
		manyStr["k"+strconv.Itoa(i)] = "v" + strconv.Itoa(i)
	}

	var nilStrMap map[string]string
	var nilAnyMap map[string]any

	cases := []struct {
		name string
		val  any
	}{
		{"nilStrMap", nilStrMap},
		{"emptyStrMap", map[string]string{}},
		{"oneStrMap", map[string]string{"message": "Hello, World!"}},
		{"twoStrMap", map[string]string{"z": "last", "a": "first"}},
		{"trickyStrMap", map[string]string{"key<" + trickyStr: trickyStr}},
		{"manyStrMap", manyStr},
		{"nilAnyMap", nilAnyMap},
		{"emptyAnyMap", map[string]any{}},
		{"oneAnyMap", map[string]any{"ok": true}},
		{"floatsAnyMap", map[string]any{
			"f64":    3.14159,
			"fzero":  0.0,
			"fneg0":  math.Copysign(0, -1),
			"fsmall": 1e-6,
			"fbig":   1e20,
			"score":  98.5,
		}},
		{"floatsExponentFallback", map[string]any{
			"ftiny": 1e-7,
			"fhuge": 1e21,
		}},
		{"mixedAnyMap", map[string]any{
			"str":    trickyStr,
			"int":    -42,
			"int32":  int32(-7),
			"int64":  int64(9007199254740991),
			"uint":   uint(7),
			"uint32": uint32(99),
			"uint64": uint64(18446744073709551615),
			"f64":    3.14159,
			"btrue":  true,
			"bfalse": false,
			"null":   nil,
		}},
		{"nestedAnyMap", map[string]any{
			"user": sampleStruct{ID: 7, Name: "Alice", OK: true},
			"tags": []string{"go", "http"},
		}},
		{"struct", sampleStruct{ID: 42, Name: trickyStr, OK: true}},
		{"slice", []int{1, 2, 3}},
		{"primitive", "plain"},
	}

	ctx := acquireContext()
	defer releaseContext(ctx)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("json.Marshal failed unexpectedly: %v", err)
			}
			resetContextInPlace(ctx)
			if err := ctx.JSON(tc.val); err != nil {
				t.Fatalf("ctx.JSON failed unexpectedly: %v", err)
			}
			if !bytes.Equal(ctx.Res.Body, want) {
				t.Fatalf("mismatch:\ngot:  %q\nwant: %q", ctx.Res.Body, want)
			}
			if ctx.Res.Status != 200 {
				t.Fatalf("status = %d, want 200", ctx.Res.Status)
			}
		})
	}

	// Error cases must return error, set status 400 and error JSON body,
	// and leave the reused Context healthy for subsequent valid JSON calls.
	errCases := []any{
		make(chan int),
		map[string]any{"bad": math.NaN()},
		map[string]any{"inf": math.Inf(1)},
		map[string]any{"ch": make(chan int)},
	}
	for i, bad := range errCases {
		resetContextInPlace(ctx)
		err := ctx.JSON(bad)
		if err == nil {
			t.Fatalf("errCase %d: expected error, got nil", i)
		}
		if ctx.Res.Status != 400 {
			t.Fatalf("errCase %d: status = %d, want 400", i, ctx.Res.Status)
		}
		if string(ctx.Res.Body) != `{"message":"error parsing json"}` {
			t.Fatalf("errCase %d: body = %q", i, ctx.Res.Body)
		}
	}

	// Verify Context still works after errors.
	resetContextInPlace(ctx)
	if err := ctx.JSON(map[string]string{"ok": "yes"}); err != nil {
		t.Fatalf("post-error JSON failed: %v", err)
	}
	if string(ctx.Res.Body) != `{"ok":"yes"}` {
		t.Fatalf("post-error body = %q", ctx.Res.Body)
	}
}

func TestJSONAndWriteStringZeroAlloc(t *testing.T) {
	ctx := acquireContext()
	defer releaseContext(ctx)

	strMap := map[string]string{"id": "42", "name": "Alice"}
	anyMap := map[string]any{"id": 42, "name": "Alice", "active": true}
	type msg struct {
		Message string `json:"message"`
	}
	st := msg{Message: "Hello, World!"}

	// Warm up jsonEnc / bodyBuf once.
	_ = ctx.JSON(st)
	resetContextInPlace(ctx)

	if a := testing.AllocsPerRun(100, func() {
		resetContextInPlace(ctx)
		_ = ctx.WriteString("Hello, World!")
	}); a != 0 {
		t.Fatalf("WriteString allocs = %v, want 0", a)
	}

	if a := testing.AllocsPerRun(100, func() {
		resetContextInPlace(ctx)
		_ = ctx.JSON(strMap)
	}); a != 0 {
		t.Fatalf("JSON(map[string]string) allocs = %v, want 0", a)
	}

	if a := testing.AllocsPerRun(100, func() {
		resetContextInPlace(ctx)
		_ = ctx.JSON(anyMap)
	}); a != 0 {
		t.Fatalf("JSON(map[string]any) allocs = %v, want 0", a)
	}

	if a := testing.AllocsPerRun(100, func() {
		resetContextInPlace(ctx)
		_ = ctx.JSON(&st)
	}); a != 0 {
		t.Fatalf("JSON(*struct) allocs = %v, want 0", a)
	}
}

func TestInlineParamsLifecycleAndOverflow(t *testing.T) {
	r := NewRouter()
	r.Handle(GET, "/a/:p1/b/:p2/c/:p3", func(c *Context) error { return nil })
	r.Handle(GET, "/1/:a/2/:b/3/:c/4/:d/5/:e/6/:f/7/:g/8/:h/9/:i", func(c *Context) error { return nil })

	ctx := acquireContext()
	defer releaseContext(ctx)

	req3 := &HTTPRequest{Method: GET, Path: "/a/10/b/20/c/30"}
	chain, n, overflow, _ := r.findDispatchInto(req3, &ctx.inlineParams)
	if chain == nil || n != 3 || overflow != nil {
		t.Fatalf("3-param route: chain=%v n=%d overflow=%v", chain != nil, n, overflow)
	}
	ctx.numParams = n
	if ctx.Param("p1") != "10" || ctx.GetParam("p2") != "20" || ctx.Param("p3") != "30" || ctx.Param("missing") != "" {
		t.Fatalf("unexpected inline params: %q %q %q", ctx.Param("p1"), ctx.Param("p2"), ctx.Param("p3"))
	}

	// Binding must read inline params cleanly.
	type bindDst struct {
		P1 int    `param:"p1"`
		P2 string `param:"p2"`
	}
	var dst bindDst
	if err := ctx.Bind(&dst); err != nil {
		t.Fatalf("Bind failed: %v", err)
	}
	if dst.P1 != 10 || dst.P2 != "20" {
		t.Fatalf("Bind got %+v, want {P1:10 P2:20}", dst)
	}

	// GetParams returns an independent copy.
	cp := ctx.GetParams()
	cp["p1"] = "mutated"
	if ctx.Param("p1") != "10" {
		t.Fatalf("GetParams copy mutated inlineParams")
	}

	// SetParam updates existing inline key and overflows cleanly past maxInlineParams.
	ctx.SetParam("p1", "11")
	if ctx.Param("p1") != "11" {
		t.Fatalf("SetParam update failed: got %q", ctx.Param("p1"))
	}
	for i := 0; i < maxInlineParams+2; i++ {
		ctx.SetParam("extra"+strconv.Itoa(i), strconv.Itoa(i))
	}
	if ctx.params == nil || ctx.numParams != 0 {
		t.Fatalf("expected overflow to map: params=%v numParams=%d", ctx.params, ctx.numParams)
	}
	if ctx.Param("p1") != "11" || ctx.Param("extra9") != "9" {
		t.Fatalf("overflow values lost: p1=%q extra9=%q", ctx.Param("p1"), ctx.Param("extra9"))
	}

	// 9-param route directly overflows to pooled map.
	resetContextInPlace(ctx)
	if ctx.Param("p1") != "" || ctx.numParams != 0 || ctx.params != nil {
		t.Fatalf("resetContextInPlace leaked params")
	}
	req9 := &HTTPRequest{Method: GET, Path: "/1/v1/2/v2/3/v3/4/v4/5/v5/6/v6/7/v7/8/v8/9/v9"}
	chain, n, overflow, _ = r.findDispatchInto(req9, &ctx.inlineParams)
	if chain == nil || n != 0 || len(overflow) != 9 {
		t.Fatalf("9-param route: chain=%v n=%d len(overflow)=%d", chain != nil, n, len(overflow))
	}
	ctx.params = overflow
	if ctx.Param("a") != "v1" || ctx.Param("i") != "v9" {
		t.Fatalf("9-param values wrong: a=%q i=%q", ctx.Param("a"), ctx.Param("i"))
	}

	// Zero allocs on inline route dispatch + Param read.
	resetContextInPlace(ctx)
	if a := testing.AllocsPerRun(100, func() {
		_, np, ov, _ := r.findDispatchInto(req3, &ctx.inlineParams)
		ctx.numParams = np
		ctx.params = ov
		_ = ctx.Param("p1")
		_ = ctx.Param("p2")
		_ = ctx.Param("p3")
		resetContextInPlace(ctx)
	}); a != 0 {
		t.Fatalf("findDispatchInto + Param allocs = %v, want 0", a)
	}
}

func TestHeaderSlabImmutability(t *testing.T) {
	type stashed struct {
		path string
		host string
	}
	saved := make([]stashed, 0, 200)

	// Cross several 4 KiB slabs on a single reused pooled HTTPRequest.
	req := acquireRequest()
	defer releaseRequest(req)

	for i := 0; i < 200; i++ {
		raw := []byte("GET /item/" + strconv.Itoa(i) + " HTTP/1.1\r\nHost: host-" + strconv.Itoa(i) + ".example\r\nX-Seq: " + strconv.Itoa(i) + "\r\n\r\n")
		consumed, err := fillHTTPRequest(req, raw, true, 0)
		if err != nil || consumed != len(raw) {
			t.Fatalf("iter %d: consumed=%d err=%v", i, consumed, err)
		}
		saved = append(saved, stashed{path: req.Path, host: req.Header["host"]})
		// Overwrite input buffer immediately to verify ownership.
		for j := range raw {
			raw[j] = 'Z'
		}
		resetRequest(req)
	}

	for i, s := range saved {
		wantPath := "/item/" + strconv.Itoa(i)
		wantHost := "host-" + strconv.Itoa(i) + ".example"
		if s.path != wantPath || s.host != wantHost {
			t.Fatalf("iter %d mutated: got (%q, %q), want (%q, %q)", i, s.path, s.host, wantPath, wantHost)
		}
	}
}

func TestQueryFastPathParityAndAllocs(t *testing.T) {
	queries := []string{
		"",
		"&&&",
		"a=1",
		"category=books&page=2&limit=50&sort=price",
		"a=1&a=2&b=3&a=4",
		"flag&empty=&=val&&&k=v",
		"a=1=2=3&b==c",
		"q=hello+world&p=%2Ffoo%20bar",
		"a=1;b=2",
		"bad=%ZZ",
		strings.Repeat("k=v&", 20) + "last=1",
		strings.Repeat("&", 20) + "a=1",
		"long=" + strings.Repeat("x", 300),
	}

	for _, raw := range queries {
		got, errGot := parseRequestQuery(raw)
		want, errWant := url.ParseQuery(raw)
		if (errGot != nil) != (errWant != nil) {
			t.Fatalf("query %q: errGot=%v errWant=%v", raw, errGot, errWant)
		}
		if len(got) != len(want) {
			t.Fatalf("query %q: len(got)=%d len(want)=%d", raw, len(got), len(want))
		}
		for k, wv := range want {
			gv, ok := got[k]
			if !ok || len(gv) != len(wv) {
				t.Fatalf("query %q key %q: got=%v want=%v", raw, k, gv, wv)
			}
			for i := range wv {
				if gv[i] != wv[i] {
					t.Fatalf("query %q key %q[%d]: got=%q want=%q", raw, k, i, gv[i], wv[i])
				}
			}
		}
	}

	// Plain queries allocate only a fresh url.Values map and a single backing
	// []string array — never pooling/clearing a map that could escape, while
	// still allocating strictly fewer objects than url.ParseQuery.
	const sample = "category=books&page=2&limit=50&sort=price"
	fastAllocs := testing.AllocsPerRun(100, func() {
		q, err := parseRequestQuery(sample)
		if err != nil || q.Get("sort") != "price" {
			t.Fatal(err)
		}
	})
	stdAllocs := testing.AllocsPerRun(100, func() {
		q, err := url.ParseQuery(sample)
		if err != nil || q.Get("sort") != "price" {
			t.Fatal(err)
		}
	})
	if fastAllocs >= stdAllocs {
		t.Fatalf("parseRequestQuery allocs = %v, expected strictly fewer than url.ParseQuery (%v)", fastAllocs, stdAllocs)
	}
}

func TestQueryMapLifetimeAcrossContextReuse(t *testing.T) {
	type queryCase struct {
		name  string
		rawA  string
		wantA url.Values
		keyA  string // key whose underlying slice is also retained separately
		rawB  string
		wantB url.Values
	}

	cases := []queryCase{
		{
			name:  "plain",
			rawA:  "GET /search?user=alice&role=admin&page=1 HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantA: url.Values{"user": {"alice"}, "role": {"admin"}, "page": {"1"}},
			keyA:  "user",
			rawB:  "GET /search?user=bob&role=guest&extra=2 HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantB: url.Values{"user": {"bob"}, "role": {"guest"}, "extra": {"2"}},
		},
		{
			name:  "duplicate_keys",
			rawA:  "GET /items?tag=go&tag=http&tag=perf&keep=1 HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantA: url.Values{"tag": {"go", "http", "perf"}, "keep": {"1"}},
			keyA:  "tag",
			rawB:  "GET /items?tag=rust&tag=tcp&keep=2 HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantB: url.Values{"tag": {"rust", "tcp"}, "keep": {"2"}},
		},
		{
			name:  "empty_values_and_valueless_keys",
			rawA:  "GET /flags?flag&empty=&=leading&k=v&tail= HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantA: url.Values{"flag": {""}, "empty": {""}, "": {"leading"}, "k": {"v"}, "tail": {""}},
			keyA:  "empty",
			rawB:  "GET /flags?flag=1&empty=filled&=other&k=w HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantB: url.Values{"flag": {"1"}, "empty": {"filled"}, "": {"other"}, "k": {"w"}},
		},
		{
			name:  "escaped_values",
			rawA:  "GET /q?msg=hello+world&path=%2Fapi%2Fv1&dup=a%201&dup=b+2 HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantA: url.Values{"msg": {"hello world"}, "path": {"/api/v1"}, "dup": {"a 1", "b 2"}},
			keyA:  "dup",
			rawB:  "GET /q?msg=goodbye+moon&path=%2Fother&dup=c%203 HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantB: url.Values{"msg": {"goodbye moon"}, "path": {"/other"}, "dup": {"c 3"}},
		},
		{
			name:  "followed_by_no_query",
			rawA:  "GET /a?k=secret&empty=&dup=1&dup=2 HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantA: url.Values{"k": {"secret"}, "empty": {""}, "dup": {"1", "2"}},
			keyA:  "k",
			rawB:  "GET /plain HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantB: nil,
		},
	}

	for _, tc := range cases {
		for _, ownHeaders := range []bool{true, false} {
			mode := "owned"
			if !ownHeaders {
				mode = "zerocopy"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := acquireContext()
				defer releaseContext(ctx)

				bufA := []byte(tc.rawA)
				nA, err := fillHTTPRequest(&ctx.req, bufA, ownHeaders, 0)
				if err != nil || nA != len(bufA) {
					t.Fatalf("req A parse: n=%d err=%v", nA, err)
				}
				ctx.Req = &ctx.req
				retainedA := ctx.Req.Query
				retainedSliceA := retainedA[tc.keyA]
				wantSliceA := append([]string(nil), tc.wantA[tc.keyA]...)

				if ownHeaders {
					// Scrub input buffer immediately to verify owned strings stay valid too.
					for i := range bufA {
						bufA[i] = 'X'
					}
				}

				// Reuse the same Context in place (pipelined inline path).
				resetContextInPlace(ctx)

				bufB := []byte(tc.rawB)
				nB, err := fillHTTPRequest(&ctx.req, bufB, ownHeaders, 0)
				if err != nil || nB != len(bufB) {
					t.Fatalf("req B parse: n=%d err=%v", nB, err)
				}
				ctx.Req = &ctx.req
				gotB := ctx.Req.Query

				if !reflect.DeepEqual(retainedA, tc.wantA) {
					t.Fatalf("retained Query A mutated after B reused Context:\ngot:  %#v\nwant: %#v", retainedA, tc.wantA)
				}
				if !reflect.DeepEqual(retainedSliceA, wantSliceA) {
					t.Fatalf("retained slice A[%q] mutated:\ngot:  %#v\nwant: %#v", tc.keyA, retainedSliceA, wantSliceA)
				}
				if !reflect.DeepEqual(gotB, tc.wantB) {
					t.Fatalf("Query B mismatch:\ngot:  %#v\nwant: %#v", gotB, tc.wantB)
				}

				// Mutating retainedA must not affect gotB.
				retainedA.Set(tc.keyA, "caller-mutated")
				if !reflect.DeepEqual(gotB, tc.wantB) {
					t.Fatalf("mutating retainedA affected Query B: got %#v want %#v", gotB, tc.wantB)
				}
			})
		}
	}

	// Verify releaseContext -> acquireContext pool round-trip also leaves retained
	// Query untouched (even when releaseRequest / resetRequest runs on release).
	{
		ctxA := acquireContext()
		rawA := []byte("GET /pool?k=alpha&dup=1&dup=2&empty=&esc=a%20b HTTP/1.1\r\nHost: test\r\n\r\n")
		if _, err := fillHTTPRequest(&ctxA.req, rawA, true, 0); err != nil {
			t.Fatal(err)
		}
		ctxA.Req = &ctxA.req
		retainedA := ctxA.Req.Query
		retainedDup := retainedA["dup"]
		releaseContext(ctxA)

		ctxB := acquireContext()
		rawB := []byte("GET /pool?k=beta&dup=9&empty=nonempty HTTP/1.1\r\nHost: test\r\n\r\n")
		if _, err := fillHTTPRequest(&ctxB.req, rawB, true, 0); err != nil {
			t.Fatal(err)
		}
		ctxB.Req = &ctxB.req
		wantA := url.Values{"k": {"alpha"}, "dup": {"1", "2"}, "empty": {""}, "esc": {"a b"}}
		if !reflect.DeepEqual(retainedA, wantA) {
			t.Fatalf("retained Query A mutated after releaseContext/acquireContext:\ngot:  %#v\nwant: %#v", retainedA, wantA)
		}
		if !reflect.DeepEqual(retainedDup, []string{"1", "2"}) {
			t.Fatalf("retained dup slice mutated: %#v", retainedDup)
		}
		releaseContext(ctxB)
	}

	// End-to-end pipelined OnTraffic test: handler retains ctx.Req.Query across
	// multiple requests that share the same per-event loopCtx.
	app := New(NewRouter(), NewEventLoopWorkerPool(1))
	var captured []url.Values
	app.Router.Handle(GET, "/p", func(c *Context) error {
		captured = append(captured, c.Req.Query)
		return c.WriteString(c.Query("id"))
	})

	wire := "GET /p?id=1&tag=a&tag=b&empty= HTTP/1.1\r\nHost: test\r\n\r\n" +
		"GET /p?id=2&tag=c&q=hello+world%21 HTTP/1.1\r\nHost: test\r\n\r\n" +
		"GET /p HTTP/1.1\r\nHost: test\r\n\r\n"
	conn := &bodyLimitConn{inbound: []byte(wire)}
	if action := app.OnTraffic(conn); action != gnetplus.None {
		t.Fatalf("OnTraffic action = %v, want None", action)
	}
	if len(captured) != 3 {
		t.Fatalf("captured %d queries, want 3", len(captured))
	}
	want0 := url.Values{"id": {"1"}, "tag": {"a", "b"}, "empty": {""}}
	want1 := url.Values{"id": {"2"}, "tag": {"c"}, "q": {"hello world!"}}
	if !reflect.DeepEqual(captured[0], want0) {
		t.Fatalf("pipelined req 0 Query mutated: got %#v want %#v", captured[0], want0)
	}
	if !reflect.DeepEqual(captured[1], want1) {
		t.Fatalf("pipelined req 1 Query mutated: got %#v want %#v", captured[1], want1)
	}
	if captured[2] != nil {
		t.Fatalf("pipelined req 2 Query = %#v, want nil", captured[2])
	}
}

func TestQueryMapConcurrentAccessAfterContextReuse(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(1))

	type job struct {
		q       url.Values
		wantID  string
		wantEnc string
		tags    []string
	}
	ch := make(chan job, 64)
	errCh := make(chan string, 16)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				for iter := 0; iter < 50; iter++ {
					if got := j.q.Get("id"); got != j.wantID {
						select {
						case errCh <- "Get(id) got " + got + " want " + j.wantID:
						default:
						}
						return
					}
					if got := j.q.Encode(); got != j.wantEnc {
						select {
						case errCh <- "Encode() got " + got + " want " + j.wantEnc:
						default:
						}
						return
					}
					if len(j.tags) > 0 && !reflect.DeepEqual(j.q["tag"], j.tags) {
						select {
						case errCh <- "duplicate tag slice mismatch":
						default:
						}
						return
					}
				}
			}
		}()
	}

	app.Router.Handle(GET, "/c", func(c *Context) error {
		q := c.Req.Query
		if q != nil {
			ch <- job{
				q:       q,
				wantID:  strings.Clone(q.Get("id")),
				wantEnc: q.Encode(),
				tags:    append([]string(nil), q["tag"]...),
			}
		}
		return c.WriteString("ok")
	})

	// Build a large pipelined batch mixing plain, duplicate-key, empty-value,
	// escaped, and queryless requests on a single reused loopCtx.
	var batch strings.Builder
	for i := 0; i < 200; i++ {
		s := strconv.Itoa(i)
		switch i % 4 {
		case 0:
			batch.WriteString("GET /c?id=")
			batch.WriteString(s)
			batch.WriteString("&k=val")
			batch.WriteString(s)
			batch.WriteString("&empty= HTTP/1.1\r\nHost: test\r\n\r\n")
		case 1:
			batch.WriteString("GET /c?id=")
			batch.WriteString(s)
			batch.WriteString("&tag=a")
			batch.WriteString(s)
			batch.WriteString("&tag=b")
			batch.WriteString(s)
			batch.WriteString("&flag HTTP/1.1\r\nHost: test\r\n\r\n")
		case 2:
			batch.WriteString("GET /c?id=")
			batch.WriteString(s)
			batch.WriteString("&q=hello+")
			batch.WriteString(s)
			batch.WriteString("%20world HTTP/1.1\r\nHost: test\r\n\r\n")
		case 3:
			batch.WriteString("GET /c HTTP/1.1\r\nHost: test\r\n\r\n")
		}
	}

	conn := &bodyLimitConn{inbound: []byte(batch.String())}
	if action := app.OnTraffic(conn); action != gnetplus.None {
		t.Fatalf("OnTraffic action = %v, want None", action)
	}
	close(ch)
	wg.Wait()

	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}
}

func TestIndexCRLFCRLFParity(t *testing.T) {
	needle := []byte("\r\n\r\n")
	inputs := [][]byte{
		nil,
		[]byte(""),
		[]byte("\r\n"),
		[]byte("\r\n\r"),
		[]byte("\r\n\r\n"),
		[]byte("a\r\n\r\n"),
		[]byte("ab\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\nHost: a\r\nX: \r\r\n\n\r\n\r\n"),
		append(bytes.Repeat([]byte("Header: value\r\n"), 50), '\r', '\n'),
		bytes.Repeat([]byte("a"), 700),
	}
	for i := 0; i < 64; i++ {
		b := bytes.Repeat([]byte("x"), i)
		inputs = append(inputs, append(append([]byte(nil), b...), needle...))
	}
	for idx, in := range inputs {
		got := indexCRLFCRLF(in)
		want := bytes.Index(in, needle)
		if got != want {
			t.Fatalf("case %d (len=%d): got %d, want %d", idx, len(in), got, want)
		}
	}
}

func TestResponseRaw200AndContentLength(t *testing.T) {
	lengths := []int{0, 1, 9, 10, 99, 100, 999, 1000, 12345}
	for _, n := range lengths {
		body := strings.Repeat("a", n)
		ctx := acquireContext()
		_ = ctx.WriteString(body)
		got := ctx.Res.AppendTo(nil)
		want := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: " + strconv.Itoa(n) + "\r\n\r\n" + body
		if string(got) != want {
			t.Fatalf("len=%d mismatch:\ngot:  %q\nwant: %q", n, got, want)
		}
		// Adding a custom header must invalidate raw200 and emit the header.
		ctx.SetHeader("X-Test", "1")
		got2 := string(ctx.Res.AppendTo(nil))
		if !strings.Contains(got2, "X-Test: 1\r\n") {
			t.Fatalf("SetHeader did not invalidate raw200: %q", got2)
		}
		releaseContext(ctx)
	}

	// Non-200 status must bypass raw200.
	ctx := acquireContext()
	defer releaseContext(ctx)
	ctx.Status(201)
	_ = ctx.JSON(map[string]string{"a": "b"})
	wire := string(ctx.Res.AppendTo(nil))
	if !strings.HasPrefix(wire, "HTTP/1.1 201 Created\r\n") {
		t.Fatalf("201 status wrong: %q", wire)
	}

	// 204 must omit Content-Length and body even after WriteString.
	resetContextInPlace(ctx)
	ctx.Status(204)
	_ = ctx.WriteString("ignored")
	wire204 := string(ctx.Res.AppendTo(nil))
	if wire204 != "HTTP/1.1 204 No Content\r\nContent-Type: text/plain\r\n\r\n" {
		t.Fatalf("204 wire wrong: %q", wire204)
	}
}
