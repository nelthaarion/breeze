package breeze

import (
	"sync/atomic"
	"unsafe"
)

// staticUnion is the merge of two static header blocks (see SetHeaderStatic): the
// concatenated wire bytes and the combined lookup map. Both inputs are immutable
// per middleware instance, so the merge is the same for every request and is
// computed once instead of on each one.
//
// The inputs are retained here on purpose: the cache is keyed by their addresses,
// and holding them keeps those addresses from being recycled for different data.
type staticUnion struct {
	a, b   []byte
	am, bm map[string]string

	block  []byte
	lookup map[string]string
}

// maxStaticUnions bounds the cache. A program that builds a new pair of static
// middlewares per request would otherwise grow it without limit; past the bound
// the merge is simply not cached, which is the old behaviour.
const maxStaticUnions = 64

var staticUnions atomic.Pointer[[]*staticUnion] // copy-on-write snapshot

func mapID(m map[string]string) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&m)) }

func (u *staticUnion) is(a []byte, am map[string]string, b []byte, bm map[string]string) bool {
	return len(u.a) == len(a) && len(u.b) == len(b) &&
		unsafe.SliceData(u.a) == unsafe.SliceData(a) && unsafe.SliceData(u.b) == unsafe.SliceData(b) &&
		mapID(u.am) == mapID(am) && mapID(u.bm) == mapID(bm)
}

func staticUnionOf(a []byte, am map[string]string, b []byte, bm map[string]string) *staticUnion {
	if cur := staticUnions.Load(); cur != nil {
		for _, u := range *cur {
			if u.is(a, am, b, bm) {
				return u
			}
		}
	}
	nb := make([]byte, 0, len(a)+len(b))
	nb = append(nb, a...)
	nb = append(nb, b...)
	nm := make(map[string]string, len(am)+len(bm))
	for k, v := range am {
		nm[k] = v
	}
	for k, v := range bm {
		nm[k] = v
	}
	u := &staticUnion{a: a, b: b, am: am, bm: bm, block: nb, lookup: nm}
	for {
		cur := staticUnions.Load()
		var list []*staticUnion
		if cur != nil {
			list = *cur
			// Another event loop may have added this very pair while we were
			// merging (every loop misses on its first request). Use its entry
			// instead of appending a duplicate that would waste a slot.
			for _, e := range list {
				if e.is(a, am, b, bm) {
					return e
				}
			}
		}
		if len(list) >= maxStaticUnions {
			return u
		}
		next := make([]*staticUnion, len(list)+1)
		copy(next, list)
		next[len(list)] = u
		if staticUnions.CompareAndSwap(cur, &next) {
			return u
		}
	}
}
