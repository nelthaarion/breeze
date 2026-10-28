package breeze

import "testing"

func TestDecideNoPollYield(t *testing.T) {
	inline := NewRouter()
	inline.Handle(GET, "/a", trieNoop)
	blocking := NewRouter()
	blocking.Handle(GET, "/a", trieNoop)
	blocking.HandleBlocking(GET, "/b", trieNoop)
	index := NewRouter()
	index.autoServeRoot = true

	cases := []struct {
		name   string
		mode   PollYield
		inline bool
		r      *Router
		procs  int
		want   bool
	}{
		{"default keeps the yield", PollYieldDefault, true, inline, 4, false},
		{"auto, all inline", PollYieldAuto, true, inline, 4, true},
		{"auto, empty router", PollYieldAuto, true, NewRouter(), 2, true},
		{"auto, single P keeps the yield", PollYieldAuto, true, inline, 1, false},
		{"auto, blocking route", PollYieldAuto, true, blocking, 4, false},
		{"auto, auto index only", PollYieldAuto, true, index, 4, true},
		{"auto, inline execution off", PollYieldAuto, false, inline, 4, false},
		{"never forces no-yield", PollYieldNever, true, blocking, 1, true},
	}
	for _, c := range cases {
		if got := decideNoPollYield(c.mode, c.inline, c.r, c.procs); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
