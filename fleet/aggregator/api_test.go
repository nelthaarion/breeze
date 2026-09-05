package aggregator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/fleet"
)

func invoke(
	t *testing.T,
	router *breeze.Router,
	method breeze.Method,
	target string,
	body []byte,
	headers map[string]string,
) *breeze.Context {
	t.Helper()
	path, rawQuery, _ := strings.Cut(target, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if headers == nil {
		headers = map[string]string{}
	}
	req := &breeze.HTTPRequest{
		Method: method,
		Path:   path,
		Query:  query,
		Header: headers,
		Body:   body,
	}
	handler, middlewares, params := router.Find(req)
	if handler == nil {
		t.Fatalf("route not found: %s %s", method, path)
	}
	ctx := breeze.NewContext(method, path)
	ctx.Req = req
	ctx.SetParams(params)
	ctx.SetMiddlewareChain(middlewares, handler)
	ctx.Next()
	return ctx
}

func validAPISpans() []fleet.Span {
	return []fleet.Span{
		{
			TraceID:      strings.Repeat("a", 32),
			SpanID:       strings.Repeat("1", 16),
			Service:      "gateway",
			Route:        "/orders",
			Method:       "POST",
			Status:       200,
			StartNanoUTC: 1,
			DurationMs:   12,
			Tags:         map[string]string{"order_id": "123"},
		},
		{
			TraceID:      strings.Repeat("a", 32),
			SpanID:       strings.Repeat("2", 16),
			ParentSpanID: strings.Repeat("1", 16),
			Service:      "orders",
			Route:        "/orders",
			Method:       "POST",
			Status:       500,
			StartNanoUTC: 2,
			DurationMs:   9,
			Error:        "failed",
		},
	}
}

func TestAggregatorHTTPIngestAndRead(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.IngestToken = "write-secret"
	cfg.Username, cfg.Password = "viewer", "read-secret"
	a := InstallAggregator(nil, router, cfg)
	defer a.Close(context.Background())

	raw, _ := json.Marshal(validAPISpans())
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write(raw)
	_ = zw.Close()

	unauth := invoke(
		t,
		router,
		breeze.POST,
		"/fleet/api/spans",
		compressed.Bytes(),
		map[string]string{"content-encoding": "gzip"},
	)
	if unauth.Res.Status != 401 {
		t.Fatalf("unauth ingest status = %d", unauth.Res.Status)
	}
	accepted := invoke(
		t,
		router,
		breeze.POST,
		"/fleet/api/spans",
		compressed.Bytes(),
		map[string]string{"content-encoding": "gzip", "x-fleet-token": "write-secret"},
	)
	if accepted.Res.Status != 202 {
		t.Fatalf("ingest status = %d body=%s", accepted.Res.Status, accepted.Res.Body)
	}

	denied := invoke(t, router, breeze.GET, "/fleet/api/traces", nil, nil)
	if denied.Res.Status != 401 {
		t.Fatalf("read without basic auth = %d", denied.Res.Status)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("viewer:read-secret"))
	list := invoke(
		t,
		router,
		breeze.GET,
		"/fleet/api/traces?tag=order_id:123&status=error",
		nil,
		map[string]string{"authorization": basic},
	)
	if list.Res.Status != 200 {
		t.Fatalf("trace list = %d body=%s", list.Res.Status, list.Res.Body)
	}
	var summaries []TraceSummary
	if err := json.Unmarshal(list.Res.Body, &summaries); err != nil || len(summaries) != 1 {
		t.Fatalf("summaries = %+v err=%v body=%s", summaries, err, list.Res.Body)
	}

	detail := invoke(
		t,
		router,
		breeze.GET,
		"/fleet/api/traces/"+strings.Repeat("a", 32),
		nil,
		map[string]string{"authorization": basic},
	)
	var tr Trace
	if err := json.Unmarshal(detail.Res.Body, &tr); err != nil || tr.SpanCount != 2 {
		t.Fatalf("trace = %+v err=%v body=%s", tr, err, detail.Res.Body)
	}
	top := a.Topology().Snapshot()
	if len(top.Edges) != 1 || top.Edges[0].Caller != "gateway" || top.Edges[0].Callee != "orders" {
		t.Fatalf("topology = %+v", top)
	}
}

func TestAggregatorRejectsWholeMalformedBatch(t *testing.T) {
	router := breeze.NewRouter()
	a := InstallAggregator(nil, router, Config{})
	defer a.Close(context.Background())
	spans := validAPISpans()
	spans[1].SpanID = "bad"
	body, _ := json.Marshal(spans)
	ctx := invoke(t, router, breeze.POST, "/fleet/api/spans", body, nil)
	if ctx.Res.Status != 400 || a.Store().Stats().Spans != 0 {
		t.Fatalf("status=%d stats=%+v", ctx.Res.Status, a.Store().Stats())
	}
}

func TestAcceptSpansResolvesParentsIndependentOfBatchOrder(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		router := breeze.NewRouter()
		a := InstallAggregator(nil, router, Config{ContractValidation: false})
		spans := validAPISpans()
		if reversed {
			spans[0], spans[1] = spans[1], spans[0]
		}
		if err := a.AcceptSpans(spans); err != nil {
			t.Fatalf("reversed=%v: AcceptSpans: %v", reversed, err)
		}
		top := a.Topology().Snapshot()
		if len(top.Edges) != 1 || top.Edges[0].Caller != "gateway" ||
			top.Edges[0].Callee != "orders" {
			t.Fatalf("reversed=%v: topology = %+v", reversed, top)
		}
		if err := a.Close(context.Background()); err != nil {
			t.Fatalf("reversed=%v: close: %v", reversed, err)
		}
	}
}

func TestAcceptSpansReconcilesParentArrivingInLaterBatch(t *testing.T) {
	router := breeze.NewRouter()
	a := InstallAggregator(nil, router, Config{ContractValidation: false})
	defer a.Close(context.Background())

	traceID := strings.Repeat("b", 32)
	parentID := strings.Repeat("3", 16)
	child := fleet.Span{
		TraceID:      traceID,
		SpanID:       strings.Repeat("4", 16),
		ParentSpanID: parentID,
		Service:      "orders",
		Route:        "/orders",
		Method:       "POST",
		Status:       200,
		StartNanoUTC: 2,
		DurationMs:   9,
	}
	parent := fleet.Span{
		TraceID:      traceID,
		SpanID:       parentID,
		Service:      "gateway",
		Route:        "/orders",
		Method:       "POST",
		Status:       200,
		StartNanoUTC: 1,
		DurationMs:   12,
	}

	if err := a.AcceptSpans([]fleet.Span{child}); err != nil {
		t.Fatal(err)
	}
	if got := a.Topology().Snapshot().Edges; len(got) != 0 {
		t.Fatalf("edge before parent = %+v", got)
	}
	if err := a.AcceptSpans([]fleet.Span{parent}); err != nil {
		t.Fatal(err)
	}

	top := a.Topology().Snapshot()
	if len(top.Edges) != 1 {
		t.Fatalf("edges = %+v, want one reconciled edge", top.Edges)
	}
	if edge := top.Edges[0]; edge.Calls != 1 || edge.Caller != "gateway" ||
		edge.Callee != "orders" {
		t.Fatalf("reconciled edge = %+v", edge)
	}
	if tr, ok := a.Store().Trace(traceID); !ok || tr.SpanCount != 2 {
		t.Fatalf("stored trace = %+v, found=%v", tr, ok)
	}
}

func TestAggregatorHeartbeatAndCloseAreSafe(t *testing.T) {
	router := breeze.NewRouter()
	cfg := Config{ServiceTTL: 10 * time.Millisecond, TraceTTL: 10 * time.Millisecond}
	a := InstallAggregator(nil, router, cfg)
	body, _ := json.Marshal(fleet.Heartbeat{Service: "orders", InstanceID: "one"})
	ctx := invoke(t, router, breeze.POST, "/fleet/api/heartbeat", body, nil)
	if ctx.Res.Status != 202 {
		t.Fatalf("heartbeat = %d", ctx.Res.Status)
	}
	if got := a.Registry().Snapshot(time.Now()); len(got) != 1 {
		t.Fatalf("registry = %+v", got)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
