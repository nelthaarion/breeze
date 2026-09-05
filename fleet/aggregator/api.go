package aggregator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/fleet"
)

const maxIngestBody = 32 << 20 // 32 MiB compressed or plain, before JSON decode.

func (a *Aggregator) registerRoutes(router *breeze.Router) {
	base := strings.TrimSuffix(a.cfg.BasePath, "/")
	ingest := func(next breeze.HandlerFunc) breeze.HandlerFunc {
		return func(ctx *breeze.Context) error {
			if !ingestAuthorized(ctx, a.cfg.IngestToken) {
				writeJSON(ctx, 401, map[string]string{"error": "unauthorized"})
				return nil
			}
			next(ctx)

			return nil
		}
	}

	router.Handle(breeze.POST, base+"/api/spans", ingest(a.ingestSpans))
	router.Handle(breeze.POST, base+"/api/heartbeat", ingest(a.ingestHeartbeat))

	auth := readAuth(a.cfg)
	router.Handle(breeze.GET, base+"/api/services", a.services, auth)
	router.Handle(breeze.GET, base+"/api/traces", a.traces, auth)
	router.Handle(breeze.GET, base+"/api/traces/:id", a.trace, auth)
	router.Handle(breeze.GET, base+"/api/traces/:id/logs", a.traceLogs, auth)

	router.Handle(breeze.GET, base+"/api/topology", a.topologySnapshot, auth)
	router.Handle(breeze.GET, base+"/api/incidents", a.incidentSnapshot, auth)
	router.Handle(breeze.GET, base+"/api/violations", a.violationSnapshot, auth)
	router.Handle(breeze.GET, base+"/api/stats", a.stats, auth)
}

func (a *Aggregator) ingestSpans(ctx *breeze.Context) error {
	data, ok := ingestBody(ctx)
	if !ok {
		return nil
	}
	var spans []fleet.Span
	if err := json.Unmarshal(data, &spans); err != nil {
		writeJSON(ctx, 400, map[string]string{"error": "malformed span batch"})
		return nil
	}
	if err := a.AcceptSpans(spans); err != nil {
		writeJSON(ctx, 400, map[string]string{"error": err.Error()})
		return nil
	}
	writeJSON(ctx, 202, map[string]int{"accepted": len(spans)})

	return nil
}

// AcceptSpans validates and stores one transport-independent batch. HTTP,
// in-process events, WebSocket, and optional transports all converge here so
// validation and topology accounting cannot drift by protocol.
func (a *Aggregator) AcceptSpans(spans []fleet.Span) error {
	for i := range spans {
		if !spans[i].Valid() {
			return errors.New("invalid span identity at index " + strconv.Itoa(i))
		}
	}

	now := time.Now()
	// Index this batch before storing it. A flush may contain parent and child
	// spans together in either order; parent resolution must not depend on
	// which span happens to be iterated first.
	batchServices := make(map[string]map[string]string)
	for _, span := range spans {
		services := batchServices[span.TraceID]
		if services == nil {
			services = make(map[string]string)
			batchServices[span.TraceID] = services
		}
		services[span.SpanID] = span.Service
	}
	for _, span := range spans {
		a.store.Add(span, now)
	}
	// Resolve parent services after the whole batch is stored, then account
	// only the newly-arrived spans. Re-observing the complete trace here would
	// inflate topology call/error counters every time another batch arrived.
	byTrace := make(map[string]map[string]string, len(spans))
	for _, span := range spans {
		services, exists := byTrace[span.TraceID]
		if !exists {
			services = make(map[string]string, len(batchServices[span.TraceID]))
			if tr, found := a.store.Trace(span.TraceID); found {
				for _, root := range tr.Roots {
					collectSpanServices(root, services)
				}
			}
			for id, service := range batchServices[span.TraceID] {
				services[id] = service
			}
			byTrace[span.TraceID] = services
		}
		parentService := ""
		if span.ParentSpanID != "" {
			parentService = services[span.ParentSpanID]
		}
		a.topology.Observe(span, parentService, now)
		a.topology.MarkEdgeSeen(span, parentService, now)
		if a.contracts != nil {
			a.contracts.enqueueSpan(span, parentService)
		}
	}
	for traceID := range byTrace {
		if tr, found := a.store.Trace(traceID); found {
			a.topology.ReconcileTrace(tr, now)
		}
	}
	if a.hub != nil {
		a.hub.traceEvent(spans)
	}
	return nil
}

func collectSpanServices(node *SpanNode, dst map[string]string) {
	if node == nil {
		return
	}
	dst[node.SpanID] = node.Service
	for _, child := range node.Children {
		collectSpanServices(child, dst)
	}
}

func (a *Aggregator) ingestHeartbeat(ctx *breeze.Context) error {
	data, ok := ingestBody(ctx)
	if !ok {
		return nil
	}
	var hb fleet.Heartbeat
	if err := json.Unmarshal(data, &hb); err != nil || hb.Service == "" {
		writeJSON(ctx, 400, map[string]string{"error": "malformed heartbeat"})
		return nil
	}
	if err := a.AcceptHeartbeat(hb); err != nil {
		writeJSON(ctx, 400, map[string]string{"error": err.Error()})
		return nil
	}
	writeJSON(ctx, 202, map[string]string{"status": "accepted"})

	return nil
}

// AcceptHeartbeat is the transport-independent heartbeat sink.
func (a *Aggregator) AcceptHeartbeat(hb fleet.Heartbeat) error {
	if hb.Service == "" {
		return errors.New("heartbeat service is required")
	}
	a.registry.Observe(hb, time.Now())
	if a.contracts != nil {
		a.contracts.enqueueSchema(hb)
	}
	if a.hub != nil {
		a.hub.serviceEvent(hb)
	}
	return nil
}

func ingestBody(ctx *breeze.Context) ([]byte, bool) {
	body := ctx.Req.Body
	if len(body) > maxIngestBody {
		writeJSON(ctx, 413, map[string]string{"error": "payload too large"})
		return nil, false
	}
	if !strings.EqualFold(ctx.Req.Header["content-encoding"], "gzip") {
		return body, true
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		writeJSON(ctx, 400, map[string]string{"error": "invalid gzip payload"})
		return nil, false
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, maxIngestBody+1))
	if err != nil || len(data) > maxIngestBody {
		writeJSON(ctx, 413, map[string]string{"error": "expanded payload too large"})
		return nil, false
	}
	return data, true
}

func (a *Aggregator) services(ctx *breeze.Context) error {
	writeJSON(ctx, 200, a.registry.Snapshot(time.Now()))

	return nil
}

func (a *Aggregator) traces(ctx *breeze.Context) error {
	q := ctx.Req.Query
	filter := TraceQuery{Service: q.Get("service"), Limit: intValue(q.Get("limit")), Cursor: q.Get("cursor")}
	filter.MinServices = intValue(q.Get("min_services"))
	if filter.MinServices <= 0 {
		filter.MinServices = 2
	}
	filter.MinDurationMs, _ = strconv.ParseFloat(q.Get("min_duration_ms"), 64)
	status := q.Get("status")
	if status == "error" || status == "5xx" {
		filter.OnlyErrors = true
	} else {
		filter.Status = intValue(status)
	}
	if tag := q.Get("tag"); tag != "" {
		filter.TagKey, filter.TagValue, _ = strings.Cut(tag, ":")
	}
	page := a.store.RecentPage(filter)
	// Keep the original array response for callers that predate cursor
	// pagination. Fleet View sends page_size explicitly and receives the
	// envelope with continuation metadata.
	_, paged := q["page_size"]
	if !paged && filter.Cursor == "" {
		writeJSON(ctx, 200, page.Traces)
		return nil
	}
	writeJSON(ctx, 200, page)

	return nil
}

func (a *Aggregator) trace(ctx *breeze.Context) error {
	id := ctx.Param("id")
	if len(id) != 32 {
		writeJSON(ctx, 400, map[string]string{"error": "invalid trace id"})
		return nil
	}
	tr, ok := a.store.Trace(id)
	if !ok {
		writeJSON(ctx, 404, map[string]string{"error": "trace not found"})
		return nil
	}
	writeJSON(ctx, 200, tr)

	return nil
}

// traceLogs serves GET /fleet/api/traces/:id/logs (§9C.2).
//
// The trace must be looked up first because its service list is what decides
// who to ask — the aggregator holds no index of "which services logged for this
// trace", and building one would mean storing log metadata it deliberately does
// not keep.
func (a *Aggregator) traceLogs(ctx *breeze.Context) error {
	id := ctx.Param("id")
	if len(id) != 32 {
		writeJSON(ctx, 400, map[string]string{"error": "invalid trace id"})
		return nil
	}
	tr, ok := a.store.Trace(id)
	if !ok {
		writeJSON(ctx, 404, map[string]string{"error": "trace not found"})
		return nil
	}
	if a.logs == nil {
		// No ServiceToken configured. Reported as an explicit disabled
		// state rather than an empty log list, so the UI can say why the
		// panel is empty instead of implying nothing was logged.
		writeJSON(ctx, 200, TraceLogs{
			TraceID: id,
			Logs:    []TraceLog{},
			Sources: []LogSource{},
			Disabled: "log stitching is disabled: set the aggregator's service_token " +
				"and each service's dashboard service_token to the same value",
		})
		return nil
	}

	// The fan-out is bounded by its own per-service timeout; this ceiling
	// bounds the whole request so one slow service cannot hold a dashboard
	// connection open indefinitely.
	reqCtx, cancel := context.WithTimeout(context.Background(), logFanoutTimeout+time.Second)
	defer cancel()
	writeJSON(ctx, 200, a.logs.Collect(reqCtx, tr, a.logEndpoints(tr.Services)))

	return nil
}

func (a *Aggregator) topologySnapshot(ctx *breeze.Context) error {

	writeJSON(ctx, 200, a.topology.Snapshot())

	return nil
}
func (a *Aggregator) incidentSnapshot(ctx *breeze.Context) error {
	writeJSON(ctx, 200, Incidents(a.topology, a.cfg, time.Now()))

	return nil
}
func (a *Aggregator) stats(ctx *breeze.Context) error {
	writeJSON(ctx, 200, a.store.Stats())
	return nil
}
func (a *Aggregator) violationSnapshot(ctx *breeze.Context) error {
	writeJSON(ctx, 200, a.Violations())
	return nil
}

func intValue(s string) int { n, _ := strconv.Atoi(s); return n }

func writeJSON(ctx *breeze.Context, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = 500, []byte(`{"error":"internal encoding error"}`)
	}
	ctx.Res = &breeze.HTTPResponse{Status: status, Headers: map[string]string{"Content-Type": "application/json"}, Body: body}
}
