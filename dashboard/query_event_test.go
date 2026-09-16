package dashboard

import (
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
)

func TestPushQueryPublishesFrameworkEventWithoutBoundArgs(t *testing.T) {
	bus := events.New(events.Config{})
	received := make(chan events.DatabaseQuery, 1)
	sub := events.OnBus(bus, events.DatabaseQuery{}, func(_ *events.Context, ev events.DatabaseQuery) error {
		received <- ev
		return nil
	})
	defer sub.Unsubscribe()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Queries = true
	c := newCollector(cfg, nil)
	c.eventsMu.Lock()
	c.eventBus = bus
	c.eventsMu.Unlock()

	c.PushQuery("SELECT * FROM users WHERE password = ?", []any{"secret-value"}, 2500, 3, "users.go", 77, nil)

	select {
	case ev := <-received:
		if ev.SQL != "SELECT * FROM users WHERE password = ?" {
			t.Fatalf("SQL = %q", ev.SQL)
		}
		// Bound query arguments are intentionally omitted from the framework event
		// to avoid leaking credentials/secrets to global event observers.
		if ev.DurationUS != 2500 || ev.Rows != 3 || ev.File != "users.go" || ev.Line != 77 {
			t.Fatalf("unexpected event metadata: %#v", ev)
		}
		if ev.Time != c.Queries(1)[0].Time.UnixNano() {
			t.Fatalf("event time does not match stored query")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for database query event")
	}
}

func TestPushQueryDoesNotEmitWhenQueriesDisabled(t *testing.T) {
	bus := events.New(events.Config{})
	received := make(chan struct{}, 1)
	sub := events.OnBus(bus, events.DatabaseQuery{}, func(_ *events.Context, _ events.DatabaseQuery) error {
		received <- struct{}{}
		return nil
	})
	defer sub.Unsubscribe()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Queries = false
	c := newCollector(cfg, nil)
	c.eventsMu.Lock()
	c.eventBus = bus
	c.eventsMu.Unlock()

	c.PushQuery("SELECT 1", nil, 10, 1, "", 0, nil)

	select {
	case <-received:
		t.Fatal("query event emitted while query collection is disabled")
	case <-time.After(50 * time.Millisecond):
	}
}
