package fleet

// Regression tests for the audit findings in the fleet/dashboard category:
//
//   - a handler that returns an error was traced as a success, because the
//     middleware read ctx.Res.Status before handleChainError had written it;
//   - heartbeat rps/error-rate were derived from *sampled* spans, so at a low
//     sample rate a healthy service reported a wildly inflated error rate;
//   - payload redaction matched a dozen exact key spellings only.

import (
	"context"
	"errors"
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

func TestMiddlewareTracesAReturnedErrorAsAFailure(t *testing.T) {
	spans, _ := traced(t, sampledConfig(), newRequest(breeze.GET, "/boom", nil),
		func(ctx *breeze.Context) error { return errors.New("db exploded") })
	s := exactlyOne(t, spans)
	if s.Status != 500 {
		t.Fatalf("span status = %d, want 500 for a handler that returned an error", s.Status)
	}
	if s.Error == "" || !s.Failed() {
		t.Fatalf("span must be marked failed, got Error=%q", s.Error)
	}
}

func TestMiddlewareTracesAnHTTPErrorWithItsOwnStatus(t *testing.T) {
	spans, _ := traced(t, sampledConfig(), newRequest(breeze.GET, "/missing", nil),
		func(ctx *breeze.Context) error { return breeze.NewHTTPError(404, "nope") })
	s := exactlyOne(t, spans)
	if s.Status != 404 {
		t.Fatalf("span status = %d, want 404", s.Status)
	}
	if s.Failed() {
		t.Fatalf("a 404 is a client error, not a failed span")
	}
}

// §7: errors are always exported even when the request was not sampled. Before
// the fix a `return err` looked like a 0 → success and was silently dropped.
func TestUnsampledReturnedErrorIsStillExported(t *testing.T) {
	cfg := TracerConfig{Enabled: true, SampleRate: 0}
	spans, _ := traced(t, cfg, newRequest(breeze.GET, "/boom", nil),
		func(ctx *breeze.Context) error { return errors.New("kaput") })
	if len(spans) != 1 || spans[0].Status != 500 {
		t.Fatalf("an unsampled failing request must still be exported with 500, got %+v", spans)
	}
}

func TestHeartbeatErrorRateIsNotSkewedBySampling(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, SampleRate: 0, ServiceName: "svc",
		AggregatorURL: "http://aggregator", Transport: rt,
	})
	defer closeTracer(t, tr)

	run := func(h breeze.HandlerFunc) {
		ctx := breeze.NewContext(breeze.GET, "/x")
		ctx.Req = newRequest(breeze.GET, "/x", nil)
		ctx.SetMiddlewareChain([]breeze.HandlerFunc{Middleware(tr)}, h)
		_ = ctx.Next()
	}
	for i := 0; i < 99; i++ {
		run(ok)
	}
	run(func(ctx *breeze.Context) error { return errors.New("one real failure") })

	tr.sendHeartbeat(context.Background())
	hbs := rt.exportedHeartbeats()
	if len(hbs) == 0 {
		t.Fatal("no heartbeat sent")
	}
	hb := hbs[len(hbs)-1]
	// 1 failure in 100 requests. Counting recorded spans instead would give
	// 1 error in 1 recorded span = 100%.
	if hb.ErrorRate < 0.009 || hb.ErrorRate > 0.011 {
		t.Fatalf("ErrorRate = %v, want ~0.01 (1 of 100 requests)", hb.ErrorRate)
	}
	if hb.RPS <= 0 {
		t.Fatalf("RPS = %v, want > 0", hb.RPS)
	}
}

func TestPayloadRedactionCoversKeyVariants(t *testing.T) {
	body := []byte(`{
		"accessToken":"a","new_password":"b","clientSecret":"c","idToken":"d",
		"X-Api-Key":"e","cvv":"123","Authorization":"f","credit_card":"g",
		"profile":{"refreshToken":"h","name":"visible","shipping":"visible"},
		"list":[{"passwd":"i"}]
	}`)
	out := string(CaptureJSONPayload(body))
	for _, leaked := range []string{`"a"`, `"b"`, `"c"`, `"d"`, `"e"`, `"123"`, `"f"`, `"g"`, `"h"`, `"i"`} {
		if contains(out, ":"+leaked) {
			t.Errorf("secret value %s survived redaction: %s", leaked, out)
		}
	}
	if !contains(out, `"name":"visible"`) || !contains(out, `"shipping":"visible"`) {
		t.Errorf("non-sensitive fields must stay readable: %s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
