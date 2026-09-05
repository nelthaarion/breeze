package dashboard

import (
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/nelthaarion/breeze"
)

// mockWriter is a minimal DBWriter used across db_writer_test.go.
type mockWriter struct {
	insertErr error
	updateErr error
	deleteErr error
}

func (m *mockWriter) InsertRow(table string, values map[string]any) (map[string]any, error) {
	if m.insertErr != nil {
		return nil, m.insertErr
	}
	out := map[string]any{"id": "1"}
	for k, v := range values {
		out[k] = v
	}
	return out, nil
}

func (m *mockWriter) UpdateRow(table string, pk map[string]any, values map[string]any) error {
	return m.updateErr
}

func (m *mockWriter) DeleteRow(table string, pk map[string]any) error {
	return m.deleteErr
}

// TestSetDBWriter_Nil verifies that passing nil to SetDBWriter clears the
// writer (no nil pointer panic), mirroring TestSetDBInspector_Nil in
// cached_inspector_test.go.
func TestSetDBWriter_Nil(t *testing.T) {
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.SetDBWriter(&mockWriter{})
	if c.DBWriter() == nil {
		t.Fatal("writer not set")
	}
	c.SetDBWriter(nil)
	if c.DBWriter() != nil {
		t.Fatal("writer not cleared")
	}
}

// TestConfigAllowWritesDefaultsFalse verifies AllowWrites is false unless
// explicitly set, so upgrading breeze never silently makes data editable.
func TestConfigAllowWritesDefaultsFalse(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.AllowWrites {
		t.Error("AllowWrites should default to false")
	}
	cfg2 := DefaultConfig()
	if cfg2.AllowWrites {
		t.Error("DefaultConfig().AllowWrites should be false")
	}
}

// TestHandleDBTableData_WritableFlag verifies the "writable" JSON field
// reflects both the AllowWrites config and whether a DBWriter is set.
func TestHandleDBTableData_WritableFlag(t *testing.T) {
	cases := []struct {
		name        string
		allowWrites bool
		setWriter   bool
		want        bool
	}{
		{"disabled by default", false, false, false},
		{"writer set but AllowWrites false", false, true, false},
		{"AllowWrites true but no writer", true, false, false},
		{"both set", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.AllowWrites = tc.allowWrites
			c := newCollector(cfg, nil)
			c.SetDBInspector(&mockInspector{})
			if tc.setWriter {
				c.SetDBWriter(&mockWriter{})
			}

			ctx := breeze.NewContext(breeze.GET, "/api/db/tables/users")
			ctx.SetParam("name", "users")
			c.handleDBTableData(ctx)

			var data TableData
			if err := json.Unmarshal(ctx.Res.Body, &data); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if data.Writable != tc.want {
				t.Errorf("Writable = %v, want %v", data.Writable, tc.want)
			}
		})
	}
}

func TestParsePK(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]any
	}{
		{"", map[string]any{}},
		{"id=42", map[string]any{"id": "42"}},
		{"a=1,b=2", map[string]any{"a": "1", "b": "2"}},
		{"name=John%20Doe", map[string]any{"name": "John Doe"}},
	}
	for _, c := range cases {
		got := parsePK(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("parsePK(%q) = %v, want %v", c.in, got, c.want)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parsePK(%q)[%q] = %v, want %v", c.in, k, got[k], v)
			}
		}
	}
}

func TestHandleDBTableInsert(t *testing.T) {
	newCollectorWithWriter := func(allowWrites bool, writer DBWriter) *Collector {
		cfg := DefaultConfig()
		cfg.AllowWrites = allowWrites
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		if writer != nil {
			c.SetDBWriter(writer)
		}
		return c
	}

	t.Run("success", func(t *testing.T) {
		c := newCollectorWithWriter(true, &mockWriter{})
		ctx := breeze.NewContext(breeze.POST, "/api/db/tables/users/rows")
		ctx.SetParam("name", "users")
		ctx.Req.Body = []byte(`{"values":{"name":"Alice"}}`)

		c.handleDBTableInsert(ctx)

		if ctx.Res.Status != 201 {
			t.Fatalf("Status = %d, want 201; body=%s", ctx.Res.Status, ctx.Res.Body)
		}
		var row map[string]any
		if err := json.Unmarshal(ctx.Res.Body, &row); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if row["name"] != "Alice" {
			t.Errorf("row[name] = %v, want Alice", row["name"])
		}
	})

	t.Run("writes disabled", func(t *testing.T) {
		c := newCollectorWithWriter(false, &mockWriter{})
		ctx := breeze.NewContext(breeze.POST, "/api/db/tables/users/rows")
		ctx.SetParam("name", "users")
		ctx.Req.Body = []byte(`{"values":{"name":"Alice"}}`)

		c.handleDBTableInsert(ctx)

		if ctx.Res.Status != 403 {
			t.Fatalf("Status = %d, want 403", ctx.Res.Status)
		}
	})

	t.Run("no writer configured", func(t *testing.T) {
		c := newCollectorWithWriter(true, nil)
		ctx := breeze.NewContext(breeze.POST, "/api/db/tables/users/rows")
		ctx.SetParam("name", "users")
		ctx.Req.Body = []byte(`{"values":{"name":"Alice"}}`)

		c.handleDBTableInsert(ctx)

		if ctx.Res.Status != 403 {
			t.Fatalf("Status = %d, want 403", ctx.Res.Status)
		}
	})

	t.Run("unknown table", func(t *testing.T) {
		c := newCollectorWithWriter(true, &mockWriter{})
		ctx := breeze.NewContext(breeze.POST, "/api/db/tables/ghost/rows")
		ctx.SetParam("name", "ghost")
		ctx.Req.Body = []byte(`{"values":{"name":"Alice"}}`)

		c.handleDBTableInsert(ctx)

		if ctx.Res.Status != 400 {
			t.Fatalf("Status = %d, want 400", ctx.Res.Status)
		}
	})

	t.Run("writer error", func(t *testing.T) {
		c := newCollectorWithWriter(true, &mockWriter{insertErr: errors.New("constraint violation")})
		ctx := breeze.NewContext(breeze.POST, "/api/db/tables/users/rows")
		ctx.SetParam("name", "users")
		ctx.Req.Body = []byte(`{"values":{"name":"Alice"}}`)

		c.handleDBTableInsert(ctx)

		if ctx.Res.Status != 400 {
			t.Fatalf("Status = %d, want 400", ctx.Res.Status)
		}
	})

	t.Run("cache invalidated and log recorded on success", func(t *testing.T) {
		mock := &mockInspector{}
		cfg := DefaultConfig()
		cfg.AllowWrites = true
		c := newCollector(cfg, nil)
		c.SetDBInspector(mock)
		c.SetDBWriter(&mockWriter{})

		// Prime the cache for "users".
		ins := c.DBInspector()
		_, _ = ins.TableData("users", 1, 50, "")
		if calls := atomic.LoadInt32(&mock.dataCalls); calls != 1 {
			t.Fatalf("dataCalls = %d, want 1 after priming cache", calls)
		}

		ctx := breeze.NewContext(breeze.POST, "/api/db/tables/users/rows")
		ctx.SetParam("name", "users")
		ctx.Req.Body = []byte(`{"values":{"name":"Alice"}}`)
		c.handleDBTableInsert(ctx)

		// A subsequent read must miss the cache (it was invalidated).
		_, _ = ins.TableData("users", 1, 50, "")
		if calls := atomic.LoadInt32(&mock.dataCalls); calls != 2 {
			t.Errorf("dataCalls = %d, want 2 (cache should have been invalidated)", calls)
		}

		logs := c.Logs("app", 0)
		if len(logs) != 1 {
			t.Fatalf("Logs(app) = %d entries, want 1", len(logs))
		}
	})
}

func TestHandleDBTableUpdate(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.AllowWrites = true
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		c.SetDBWriter(&mockWriter{})

		ctx := breeze.NewContext(breeze.PUT, "/api/db/tables/users/rows/id=1")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", "id=1")
		ctx.Req.Body = []byte(`{"values":{"name":"Bob"}}`)

		c.handleDBTableUpdate(ctx)

		if ctx.Res.Status != 200 {
			t.Fatalf("Status = %d, want 200; body=%s", ctx.Res.Status, ctx.Res.Body)
		}
	})

	t.Run("row not found", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.AllowWrites = true
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		c.SetDBWriter(&mockWriter{updateErr: ErrRowNotFound})

		ctx := breeze.NewContext(breeze.PUT, "/api/db/tables/users/rows/id=999")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", "id=999")
		ctx.Req.Body = []byte(`{"values":{"name":"Bob"}}`)

		c.handleDBTableUpdate(ctx)

		if ctx.Res.Status != 404 {
			t.Fatalf("Status = %d, want 404", ctx.Res.Status)
		}
	})

	t.Run("writes disabled", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.AllowWrites = false
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		c.SetDBWriter(&mockWriter{})

		ctx := breeze.NewContext(breeze.PUT, "/api/db/tables/users/rows/id=1")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", "id=1")
		ctx.Req.Body = []byte(`{"values":{"name":"Bob"}}`)

		c.handleDBTableUpdate(ctx)

		if ctx.Res.Status != 403 {
			t.Fatalf("Status = %d, want 403", ctx.Res.Status)
		}
	})

	t.Run("writer error", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.AllowWrites = true
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		c.SetDBWriter(&mockWriter{updateErr: errors.New("constraint violation")})

		ctx := breeze.NewContext(breeze.PUT, "/api/db/tables/users/rows/id=1")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", "id=1")
		ctx.Req.Body = []byte(`{"values":{"name":"Bob"}}`)

		c.handleDBTableUpdate(ctx)

		if ctx.Res.Status != 400 {
			t.Fatalf("Status = %d, want 400", ctx.Res.Status)
		}
	})
}

func TestHandleDBTableDelete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.AllowWrites = true
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		c.SetDBWriter(&mockWriter{})

		ctx := breeze.NewContext(breeze.DELETE, "/api/db/tables/users/rows/id=1")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", "id=1")

		c.handleDBTableDelete(ctx)

		if ctx.Res.Status != 204 {
			t.Fatalf("Status = %d, want 204; body=%s", ctx.Res.Status, ctx.Res.Body)
		}
		if len(ctx.Res.Body) != 0 {
			t.Errorf("Body = %q, want empty for 204", ctx.Res.Body)
		}
	})

	t.Run("row not found", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.AllowWrites = true
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		c.SetDBWriter(&mockWriter{deleteErr: ErrRowNotFound})

		ctx := breeze.NewContext(breeze.DELETE, "/api/db/tables/users/rows/id=999")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", "id=999")

		c.handleDBTableDelete(ctx)

		if ctx.Res.Status != 404 {
			t.Fatalf("Status = %d, want 404", ctx.Res.Status)
		}
	})

	t.Run("writes disabled", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.AllowWrites = false
		c := newCollector(cfg, nil)
		c.SetDBInspector(&mockInspector{})
		c.SetDBWriter(&mockWriter{})

		ctx := breeze.NewContext(breeze.DELETE, "/api/db/tables/users/rows/id=1")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", "id=1")

		c.handleDBTableDelete(ctx)

		if ctx.Res.Status != 403 {
			t.Fatalf("Status = %d, want 403", ctx.Res.Status)
		}
	})
}

// pkCapturingWriter is a DBWriter that records the pk map it was called
// with, so TestDBWriteRoutes_ThroughRouter can assert on exactly what
// survived the real router's path-splitting + parsePK's percent-decoding.
type pkCapturingWriter struct {
	lastUpdatePK map[string]any
	lastDeletePK map[string]any
}

func (w *pkCapturingWriter) InsertRow(table string, values map[string]any) (map[string]any, error) {
	out := map[string]any{"id": "1"}
	for k, v := range values {
		out[k] = v
	}
	return out, nil
}

func (w *pkCapturingWriter) UpdateRow(table string, pk map[string]any, values map[string]any) error {
	w.lastUpdatePK = pk
	return nil
}

func (w *pkCapturingWriter) DeleteRow(table string, pk map[string]any) error {
	w.lastDeletePK = pk
	return nil
}

// TestDBWriteRoutes_ThroughRouter is the router-level integration test
// called for in the design spec's "Testing" section. Every other test in
// this file calls the handleDBTable* methods directly with a hand-set
// ctx.SetParam(...), which never exercises breeze.Router.Find — so nothing
// else in the suite verifies that the three write routes are actually
// registered at the right method+pattern, or that a real URL path is split
// into segments and :pk is extracted the same way production traffic would
// see it.
//
// This test builds a real *breeze.Router, installs the dashboard onto it
// via Install (which calls registerRoutes internally, registering the
// production POST/PUT/DELETE .../rows[...] routes), then drives each write
// route through router.Find with a literal URL path string — mirroring
// exactly how breeze.go's OnTraffic dispatches a resolved handler
// (handler, middlewares, params := router.Find(req); then
// ctx.middlewares = append(middlewares, handler); ctx.Next()).
func TestDBWriteRoutes_ThroughRouter(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true // bypass Basic Auth so the test doesn't need auth headers
	cfg.AllowWrites = true

	c := Install(nil, router, cfg)
	c.SetDBInspector(&mockInspector{})
	writer := &pkCapturingWriter{}
	c.SetDBWriter(writer)

	const rowsPath = "/dashboard/api/db/tables/users/rows"

	invoke := func(req *breeze.HTTPRequest) *breeze.Context {
		handler, middlewares, params := router.Find(req)
		if handler == nil {
			t.Fatalf("router.Find(%s %s) did not resolve to a handler", req.Method, req.Path)
		}
		ctx := breeze.NewContext(req.Method, req.Path)
		ctx.Req = req
		ctx.SetParams(params)
		ctx.SetMiddlewareChain(middlewares, handler)
		ctx.Next()
		return ctx
	}

	// --- POST insert, through the real router ---
	insertReq := &breeze.HTTPRequest{
		Method: breeze.POST,
		Path:   rowsPath,
		Header: map[string]string{},
		Body:   []byte(`{"values":{"name":"Alice"}}`),
	}
	ctx := invoke(insertReq)
	if ctx.Res.Status != 201 {
		t.Fatalf("insert: Status = %d, want 201; body=%s", ctx.Res.Status, ctx.Res.Body)
	}
	var inserted map[string]any
	if err := json.Unmarshal(ctx.Res.Body, &inserted); err != nil {
		t.Fatalf("insert: unmarshal response: %v", err)
	}
	if inserted["name"] != "Alice" {
		t.Errorf("insert: response[name] = %v, want Alice", inserted["name"])
	}

	// --- PUT update, with a PK value containing ',' and '=' — characters
	// that are meaningful to parsePK's own splitting — so the segment must
	// travel through router.Find still percent-encoded and only get
	// decoded by parsePK, not by the router itself.
	pkValue := "a,b=c"
	pkSegment := "name=" + url.QueryEscape(pkValue)
	updatePath := rowsPath + "/" + pkSegment

	// Sanity-check router.Find extracts the raw (still-encoded) segment
	// verbatim, proving the router does not decode path segments itself.
	if _, _, params := router.Find(&breeze.HTTPRequest{Method: breeze.PUT, Path: updatePath}); params["pk"] != pkSegment {
		t.Fatalf("router.Find params[pk] = %q, want %q (still percent-encoded)", params["pk"], pkSegment)
	}

	updateReq := &breeze.HTTPRequest{
		Method: breeze.PUT,
		Path:   updatePath,
		Header: map[string]string{},
		Body:   []byte(`{"values":{"name":"Bob"}}`),
	}
	ctx = invoke(updateReq)
	if ctx.Res.Status != 200 {
		t.Fatalf("update: Status = %d, want 200; body=%s", ctx.Res.Status, ctx.Res.Body)
	}
	if want := (map[string]any{"name": pkValue}); !reflect.DeepEqual(writer.lastUpdatePK, want) {
		t.Errorf("update: writer received pk = %#v, want %#v (pk must survive router splitting + parsePK decoding intact)", writer.lastUpdatePK, want)
	}

	// --- DELETE, same PK round-trip ---
	deleteReq := &breeze.HTTPRequest{
		Method: breeze.DELETE,
		Path:   rowsPath + "/" + pkSegment,
		Header: map[string]string{},
	}
	ctx = invoke(deleteReq)
	if ctx.Res.Status != 204 {
		t.Fatalf("delete: Status = %d, want 204; body=%s", ctx.Res.Status, ctx.Res.Body)
	}
	if want := (map[string]any{"name": pkValue}); !reflect.DeepEqual(writer.lastDeletePK, want) {
		t.Errorf("delete: writer received pk = %#v, want %#v", writer.lastDeletePK, want)
	}
}
