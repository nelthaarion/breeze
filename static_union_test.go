package breeze

import (
	"reflect"
	"sync"
	"testing"
	"unsafe"
)

func TestStaticUnionCachedSharedAndCorrect(t *testing.T) {
	a := []byte("A: 1\r\n")
	am := map[string]string{"A": "1"}
	b := []byte("B: 2\r\nC: 3\r\n")
	bm := map[string]string{"B": "2", "C": "3"}
	merge := func() *Context {
		c := NewContext(GET, "/")
		c.SetHeaderStatic(a, am)
		c.SetHeaderStatic(b, bm)
		return c
	}
	c1, c2 := merge(), merge()
	if got := string(c1.Res.preBuilt); got != "A: 1\r\nB: 2\r\nC: 3\r\n" {
		t.Fatalf("merged block %q", got)
	}
	if want := (map[string]string{"A": "1", "B": "2", "C": "3"}); !reflect.DeepEqual(c1.Res.preHeaders, want) {
		t.Fatalf("merged lookup %v", c1.Res.preHeaders)
	}
	if unsafe.SliceData(c1.Res.preBuilt) != unsafe.SliceData(c2.Res.preBuilt) {
		t.Fatal("the merge was recomputed instead of shared")
	}
	if !c1.Res.headersShared {
		t.Fatal("shared union must stay copy-on-write")
	}
	// A dynamic header after the union must not write through to the shared state.
	c1.SetHeader("X-Extra", "1")
	if _, leaked := c2.Res.preHeaders["X-Extra"]; leaked {
		t.Fatal("SetHeader leaked into the shared union")
	}
	if string(a) != "A: 1\r\n" || len(am) != 1 || len(bm) != 2 {
		t.Fatal("source blocks were modified")
	}
	// Different pairs must not collide.
	d := []byte("D: 4\r\n")
	dm := map[string]string{"D": "4"}
	c3 := NewContext(GET, "/")
	c3.SetHeaderStatic(a, am)
	c3.SetHeaderStatic(d, dm)
	if got := string(c3.Res.preBuilt); got != "A: 1\r\nD: 4\r\n" {
		t.Fatalf("second pair merged to %q", got)
	}
}

func TestStaticUnionNoAllocWhenCached(t *testing.T) {
	a, am := []byte("A: 1\r\n"), map[string]string{"A": "1"}
	b, bm := []byte("B: 2\r\n"), map[string]string{"B": "2"}
	c := NewContext(GET, "/")
	r := c.ensureResponse()
	run := func() {
		r.preBuilt, r.preHeaders = nil, nil
		c.SetHeaderStatic(a, am)
		c.SetHeaderStatic(b, bm)
	}
	run()
	if n := testing.AllocsPerRun(200, run); n != 0 {
		t.Fatalf("merging two static blocks allocates %v times per request once cached", n)
	}
}

// Every event loop misses on its first request for a pair: concurrent first use
// must agree on one shared union and must not fill the cache with duplicates.
func TestStaticUnionConcurrentFirstUse(t *testing.T) {
	a, am := []byte("A: 1\r\n"), map[string]string{"A": "1"}
	b, bm := []byte("B: 2\r\n"), map[string]string{"B": "2"}
	results := make([]*staticUnion, 32)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = staticUnionOf(a, am, b, bm)
		}(i)
	}
	close(start)
	wg.Wait()
	for _, u := range results {
		if string(u.block) != "A: 1\r\nB: 2\r\n" || len(u.lookup) != 2 {
			t.Fatalf("wrong union %q %v", u.block, u.lookup)
		}
	}
	n := 0
	for _, e := range *staticUnions.Load() {
		if e.is(a, am, b, bm) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d cache entries for one pair, want 1", n)
	}
}
