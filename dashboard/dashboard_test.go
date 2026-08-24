package dashboard

import (
	"testing"
	"time"
)

func TestRingBuffer(t *testing.T) {
	rb := newRingBuffer[int](3)
	rb.Push(1)
	rb.Push(2)
	rb.Push(3)
	if got := rb.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	snap := rb.Snapshot()
	if len(snap) != 3 || snap[0] != 1 || snap[2] != 3 {
		t.Fatalf("Snapshot = %v, want [1 2 3]", snap)
	}
	// Overflow — oldest entry (1) is evicted.
	rb.Push(4)
	snap = rb.Snapshot()
	if len(snap) != 3 || snap[0] != 2 || snap[2] != 4 {
		t.Fatalf("Snapshot after overflow = %v, want [2 3 4]", snap)
	}
	rb.Clear()
	if rb.Len() != 0 {
		t.Fatalf("Len after Clear = %d, want 0", rb.Len())
	}
}

func TestMaskHeaders(t *testing.T) {
	cfg := Config{
		MaskedHeaders: []string{"Authorization", "Cookie", "X-API-Key"},
	}
	in := map[string]string{
		"Authorization": "Bearer secret",
		"Cookie":        "session=abc",
		"X-API-Key":     "key123",
		"User-Agent":    "curl/8.0",
		"Content-Type":  "application/json",
	}
	out := maskHeaders(cfg, in)
	if out["Authorization"] != "••••••" {
		t.Errorf("Authorization not masked: %q", out["Authorization"])
	}
	if out["Cookie"] != "••••••" {
		t.Errorf("Cookie not masked: %q", out["Cookie"])
	}
	if out["X-API-Key"] != "••••••" {
		t.Errorf("X-API-Key not masked: %q", out["X-API-Key"])
	}
	if out["User-Agent"] != "curl/8.0" {
		t.Errorf("User-Agent was masked: %q", out["User-Agent"])
	}
	if out["Content-Type"] != "application/json" {
		t.Errorf("Content-Type was masked: %q", out["Content-Type"])
	}
}

func TestMaskLine(t *testing.T) {
	cfg := Config{
		MaskedHeaders: []string{"token", "password"},
	}
	cases := []struct {
		in   string
		want string
	}{
		{"hello world", "hello world"},
		{"token=abc123", "token=••••••"},
		{"password:secret", "password:••••••"},
		{"password: secret", "password:•••••• secret"},
		{"no secrets here", "no secrets here"},
	}
	for _, c := range cases {
		got := maskLine(cfg, c.in)
		if got != c.want {
			t.Errorf("maskLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTimelineRecorder(t *testing.T) {
	cfg := Config{MaxTimelineEntries: 16}
	rec := NewTimelineRecorder(cfg)

	end := rec.Step("ORM Query")
	time.Sleep(2 * time.Millisecond)
	end(map[string]any{"rows": 42})

	steps := rec.Build()
	if len(steps) != 1 {
		t.Fatalf("Build() returned %d steps, want 1", len(steps))
	}
	if steps[0].Name != "ORM Query" {
		t.Errorf("Name = %q, want 'ORM Query'", steps[0].Name)
	}
	if steps[0].Duration <= 0 {
		t.Errorf("Duration = %d, want > 0", steps[0].Duration)
	}
	if steps[0].Metadata["rows"] != 42 {
		t.Errorf("Metadata[rows] = %v, want 42", steps[0].Metadata["rows"])
	}
}

func TestCollectorRequest(t *testing.T) {
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	// RecordRequest no longer updates counters or route stats (those
	// are done by the middleware). It only pushes to the ring buffer.
	c.RecordRequest(RequestRecord{
		ID:       "req-1",
		Time:     time.Now(),
		Method:   "GET",
		Path:     "/api/users",
		Route:    "/api/users",
		Status:   200,
		Duration: 1500,
	})
	recs := c.Requests(0)
	if len(recs) != 1 || recs[0].ID != "req-1" {
		t.Errorf("Requests() = %v, want one record with ID req-1", recs)
	}
}

func TestCollectorCache(t *testing.T) {
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.RecordCacheHit(true)
	c.RecordCacheHit(true)
	c.RecordCacheHit(false)
	stats := c.CacheStats()
	if stats.Hits != 2 || stats.Misses != 1 {
		t.Errorf("CacheStats = %+v, want Hits=2, Misses=1", stats)
	}
	if stats.HitRate < 0.66 || stats.HitRate > 0.67 {
		t.Errorf("HitRate = %f, want ~0.667", stats.HitRate)
	}
}

func TestCollectorQueue(t *testing.T) {
	cfg := DefaultConfig()
	c := newCollector(cfg, nil)
	c.RegisterJob(QueueJob{ID: "job-1", Queue: "emails", State: "pending"})
	c.UpdateJob("job-1", "running")
	c.UpdateJob("job-1", "failed")
	jobs := c.Jobs()
	if len(jobs) != 1 || jobs[0].State != "failed" {
		t.Errorf("Jobs() = %+v, want one failed job", jobs)
	}
	if !c.RetryJob("job-1") {
		t.Error("RetryJob returned false")
	}
	jobs = c.Jobs()
	if jobs[0].State != "pending" {
		t.Errorf("After retry, State = %q, want pending", jobs[0].State)
	}
}

func TestConfigDefaults(t *testing.T) {
	// Empty config should fill all defaults.
	cfg := Config{}.withDefaults()
	if cfg.BasePath != "/dashboard" {
		t.Errorf("BasePath = %q, want /dashboard", cfg.BasePath)
	}
	if cfg.MaxRequests != 1000 {
		t.Errorf("MaxRequests = %d, want 1000", cfg.MaxRequests)
	}
	if len(cfg.MaskedHeaders) == 0 {
		t.Error("MaskedHeaders should default to non-empty")
	}
}

func TestDecodeBasic(t *testing.T) {
	// "admin:admin" base64 = "YWRtaW46YWRtaW4="
	user, pass, ok := decodeBasic("YWRtaW46YWRtaW4=")
	if !ok {
		t.Fatal("decodeBasic returned ok=false")
	}
	if user != "admin" || pass != "admin" {
		t.Errorf("decodeBasic = (%q, %q), want (admin, admin)", user, pass)
	}
}

func TestBreezeToOpenAPIPath(t *testing.T) {
	cases := map[string]string{
		"/users":           "/users",
		"/users/:id":       "/users/{id}",
		"/users/:id/posts": "/users/{id}/posts",
		"/files/*path":     "/files/*path",
	}
	for in, want := range cases {
		got := breezeToOpenAPIPath(in)
		if got != want {
			t.Errorf("breezeToOpenAPIPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusMatch(t *testing.T) {
	cases := []struct {
		filter string
		status int
		want   bool
	}{
		{"200", 200, true},
		{"200", 404, false},
		{"2xx", 200, true},
		{"2xx", 201, true},
		{"2xx", 404, false},
		{"5xx", 503, true},
		{"5xx", 200, false},
		{"", 200, true},
	}
	for _, c := range cases {
		got := statusMatch(c.filter, c.status)
		if got != c.want {
			t.Errorf("statusMatch(%q, %d) = %v, want %v", c.filter, c.status, got, c.want)
		}
	}
}

func TestGenerateSnippets(t *testing.T) {
	req := APIExplorerExecRequest{
		Method:  "POST",
		URL:     "/api/users",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"name":"Alice"}`,
	}
	snips := generateSnippets(req)
	wantLangs := []string{"curl", "go", "javascript", "python", "csharp", "php"}
	for _, lang := range wantLangs {
		s, ok := snips[lang]
		if !ok || len(s) == 0 {
			t.Errorf("snippet for %s is missing or empty", lang)
		}
	}
	// Spot check: curl should contain the URL.
	if snips["curl"] == "" || !contains(snips["curl"], "/api/users") {
		t.Errorf("curl snippet doesn't contain URL: %q", snips["curl"])
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
