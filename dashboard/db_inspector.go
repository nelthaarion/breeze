package dashboard

import (
	"sync"
	"time"
)

// DBInspector provides database metadata and paginated table rows for the UI.
type DBInspector interface {
	Tables() ([]TableInfo, error)
	TableData(name string, page, pageSize int, search string) (TableData, error)
}

type cachedDBInspector struct {
	inner DBInspector
	ttl   time.Duration

	mu        sync.RWMutex
	tables    []TableInfo
	tablesErr error
	tablesAt  time.Time

	data    map[dbTableDataKey]cachedTableData
	dataMu  sync.RWMutex
	dataTTL time.Duration
}

type dbTableDataKey struct {
	name     string
	page     int
	pageSize int
	search   string
}

type cachedTableData struct {
	data TableData
	err  error
	at   time.Time
}

func newCachedDBInspector(inner DBInspector, ttl time.Duration) DBInspector {
	return &cachedDBInspector{
		inner:   inner,
		ttl:     ttl,
		data:    make(map[dbTableDataKey]cachedTableData),
		dataTTL: ttl,
	}
}

func (c *cachedDBInspector) Tables() ([]TableInfo, error) {
	now := time.Now()
	c.mu.RLock()
	if !c.tablesAt.IsZero() && now.Sub(c.tablesAt) < c.ttl {
		tables := make([]TableInfo, len(c.tables))
		copy(tables, c.tables)
		err := c.tablesErr
		c.mu.RUnlock()
		return tables, err
	}
	c.mu.RUnlock()

	tables, err := c.inner.Tables()
	if err != nil {
		// A failure is not cached. It used to be kept for the whole TTL, so a database
		// that was down for a second left the browser showing the error for thirty.
		return tables, err
	}

	c.mu.Lock()
	c.tables = append([]TableInfo(nil), tables...)
	c.tablesErr = nil
	c.tablesAt = now
	c.mu.Unlock()

	return tables, nil
}

func (c *cachedDBInspector) TableData(name string, page, pageSize int, search string) (TableData, error) {
	key := dbTableDataKey{name: name, page: page, pageSize: pageSize, search: search}
	now := time.Now()

	c.dataMu.RLock()
	if cached, ok := c.data[key]; ok && !cached.at.IsZero() && now.Sub(cached.at) < c.dataTTL {
		c.dataMu.RUnlock()
		return cached.data, cached.err
	}
	c.dataMu.RUnlock()

	data, err := c.inner.TableData(name, page, pageSize, search)
	if err != nil {
		return data, err
	}

	c.dataMu.Lock()
	c.pruneLocked(now)
	c.data[key] = cachedTableData{data: data, at: now}
	c.dataMu.Unlock()

	return data, nil
}

// maxCachedPages bounds the page cache. Entries were only ever treated as stale on read, never
// removed, and the key includes the page, the page size and the search text, so browsing and
// searching grew the map (each entry holding a page of rows) for as long as the process ran.
const maxCachedPages = 256

// pruneLocked drops expired entries and, if the cache is still full, the oldest. The caller
// holds dataMu for writing.
func (c *cachedDBInspector) pruneLocked(now time.Time) {
	for key, e := range c.data {
		if now.Sub(e.at) >= c.dataTTL {
			delete(c.data, key)
		}
	}
	for len(c.data) >= maxCachedPages {
		var oldest dbTableDataKey
		var oldestAt time.Time
		first := true
		for key, e := range c.data {
			if first || e.at.Before(oldestAt) {
				oldest, oldestAt, first = key, e.at, false
			}
		}
		delete(c.data, oldest)
	}
}

// cachedColumns returns the columns of a table from a page that is still fresh in the cache,
// and whether there was such a page. It never calls the underlying inspector.
func (c *cachedDBInspector) cachedColumns(table string) ([]TableColumn, bool) {
	now := time.Now()
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	found := false
	for key, e := range c.data {
		if key.name != table || e.at.IsZero() || now.Sub(e.at) >= c.dataTTL {
			continue
		}
		found = true
		if len(e.data.Columns) > 0 {
			return e.data.Columns, true
		}
	}
	return nil, found
}

// Invalidate clears cached TableData pages for the given table, forcing
// the next read to hit the underlying inspector. It does not affect the
// cached Tables() result (row counts may lag up to the cache TTL after a
// write — the same staleness the read path already tolerates).
func (c *cachedDBInspector) Invalidate(table string) {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	for key := range c.data {
		if key.name == table {
			delete(c.data, key)
		}
	}
}
