package aggregator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/transport/eventtransport"
)

func TestWSAuthRolesAndPublishDecode(t *testing.T) {
	cfg := DefaultConfig()
	a := &Aggregator{cfg: Config{Username: "viewer", Password: "read", IngestToken: "write"}, store: NewMemStore(cfg), registry: NewServiceRegistry(cfg), topology: NewTopologyGraph(cfg)}
	h := &wsHub{a: a}
	viewer := &fleetWSClient{}
	if !h.authenticate(viewer, wsEnvelope{Role: "viewer", Username: "viewer", Password: "read"}) || viewer.role != wsViewer {
		t.Fatal("viewer auth failed")
	}
	bad := &fleetWSClient{}
	if h.authenticate(bad, wsEnvelope{Role: "ingest", Token: "wrong"}) {
		t.Fatal("bad ingest token accepted")
	}
	ingest := &fleetWSClient{}
	if !h.authenticate(ingest, wsEnvelope{Role: "ingest", Token: "write"}) || ingest.role != wsIngest {
		t.Fatal("ingest auth failed")
	}

	span := fleet.Span{TraceID: strings.Repeat("d", 32), SpanID: strings.Repeat("4", 16), Service: "orders"}
	payload, _ := json.Marshal([]fleet.Span{span})
	if err := h.acceptPublish(eventtransport.DefaultSpansTopic, payload); err != nil {
		t.Fatal(err)
	}
	if a.store.Stats().Spans != 1 {
		t.Fatalf("span not accepted: %+v", a.store.Stats())
	}
}
