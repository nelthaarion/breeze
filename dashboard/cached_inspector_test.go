package dashboard

import (
	"sync/atomic"
	"testing"
	"time"
)

// mockInspector counts calls to Tables() and TableData().
type mockInspector struct {
	tablesCalls int32
	dataCalls   int32
}

func (m *mockInspector) Tables() ([]TableInfo, error) {
	atomic.AddInt32(&m.tablesCalls, 1)
	return []TableInfo{{Name: "users", Rows: 1}}, nil
}

func (m *mockInspector) TableData(name string, page, pageSize int, search string) (TableData, error) {
	atomic.AddInt32(&m.dataCalls, 1)
	return TableData{Table: name, Page: page, PageSize: pageSize, Total: 1}, nil
}

// TestCachedInspector_TablesCached verifies that Tables() is cached.
// Multiple calls within the TTL should only call the underlying inspector once.
func TestCachedInspector_TablesCached(t *testing.T) {
	mock := &mockInspector{}
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.SetDBInspector(mock)

	ins := c.DBInspector()

	// First call — should hit the underlying inspector.
	_, _ = ins.Tables()
	_, _ = ins.Tables()
	_, _ = ins.Tables()

	calls := atomic.LoadInt32(&mock.tablesCalls)
	if calls != 1 {
		t.Errorf("Tables() called %d times, want 1 (should be cached)", calls)
	}
}

// TestCachedInspector_TableDataCached verifies that TableData() is cached
// per unique (name, page, pageSize, search) combination.
func TestCachedInspector_TableDataCached(t *testing.T) {
	mock := &mockInspector{}
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.SetDBInspector(mock)

	ins := c.DBInspector()

	// Same parameters — should be cached after first call.
	_, _ = ins.TableData("users", 1, 50, "")
	_, _ = ins.TableData("users", 1, 50, "")
	_, _ = ins.TableData("users", 1, 50, "")

	calls := atomic.LoadInt32(&mock.dataCalls)
	if calls != 1 {
		t.Errorf("TableData() called %d times, want 1 (should be cached)", calls)
	}

	// Different parameters — should be a cache miss.
	_, _ = ins.TableData("users", 2, 50, "")
	calls = atomic.LoadInt32(&mock.dataCalls)
	if calls != 2 {
		t.Errorf("TableData() called %d times, want 2 (page 2 should be a miss)", calls)
	}

	// Different search — should be a cache miss.
	_, _ = ins.TableData("users", 1, 50, "alice")
	calls = atomic.LoadInt32(&mock.dataCalls)
	if calls != 3 {
		t.Errorf("TableData() called %d times, want 3 (search should be a miss)", calls)
	}
}

// TestCachedInspector_NoLockContention verifies that the cached inspector
// does not hold a lock while calling the underlying inspector.
//
// This is the KEY test for the throughput problem: if the cached inspector
// holds a mutex while calling Tables()/TableData() on the underlying
// inspector (which may take a RLock on the app's data store), it would
// block all other dashboard requests. The cache prevents this by only
// calling the underlying inspector once per TTL window.
func TestCachedInspector_NoLockContention(t *testing.T) {
	mock := &mockInspector{}
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.SetDBInspector(mock)

	ins := c.DBInspector()

	// First call populates the cache.
	_, _ = ins.Tables()

	// Subsequent calls should be instant (cached) — no lock contention
	// with the underlying inspector.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_, _ = ins.Tables()
		}
		close(done)
	}()

	select {
	case <-done:
		// good — 1000 cached calls completed quickly
	case <-time.After(time.Second):
		t.Fatal("cached Tables() calls took >1s — lock contention suspected")
	}

	calls := atomic.LoadInt32(&mock.tablesCalls)
	if calls != 1 {
		t.Errorf("underlying Tables() called %d times, want 1 (all 1000 calls should be cached)", calls)
	}
}

// TestSetDBInspector_Nil verifies that passing nil to SetDBInspector
// clears the inspector (no nil pointer panic).
func TestSetDBInspector_Nil(t *testing.T) {
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.SetDBInspector(&mockInspector{})
	if c.DBInspector() == nil {
		t.Fatal("inspector not set")
	}
	c.SetDBInspector(nil)
	if c.DBInspector() != nil {
		t.Fatal("inspector not cleared")
	}
}

// TestCachedInspector_Invalidate verifies that Invalidate(table) clears
// only that table's cached TableData pages, leaving other tables' cached
// data (and the cached Tables() list) untouched.
func TestCachedInspector_Invalidate(t *testing.T) {
	mock := &mockInspector{}
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.SetDBInspector(mock)

	ci, ok := c.dbInspector.(*cachedDBInspector)
	if !ok {
		t.Fatal("SetDBInspector did not wrap the inspector in a cachedDBInspector")
	}

	// Populate the cache for two tables.
	_, _ = ci.TableData("users", 1, 50, "")
	_, _ = ci.TableData("posts", 1, 50, "")
	if calls := atomic.LoadInt32(&mock.dataCalls); calls != 2 {
		t.Fatalf("dataCalls = %d, want 2 after populating cache", calls)
	}

	ci.Invalidate("users")

	// "users" was invalidated — this call must hit the underlying inspector.
	_, _ = ci.TableData("users", 1, 50, "")
	if calls := atomic.LoadInt32(&mock.dataCalls); calls != 3 {
		t.Errorf("dataCalls = %d, want 3 (users should be a cache miss after Invalidate)", calls)
	}

	// "posts" was NOT invalidated — this call must stay cached.
	_, _ = ci.TableData("posts", 1, 50, "")
	if calls := atomic.LoadInt32(&mock.dataCalls); calls != 3 {
		t.Errorf("dataCalls = %d, want 3 (posts should still be cached)", calls)
	}
}
