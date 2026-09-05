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

	c.mu.Lock()
	c.tables = append([]TableInfo(nil), tables...)
	c.tablesErr = err
	c.tablesAt = now
	c.mu.Unlock()

	return tables, err
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

	c.dataMu.Lock()
	c.data[key] = cachedTableData{data: data, err: err, at: now}
	c.dataMu.Unlock()

	return data, err
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
