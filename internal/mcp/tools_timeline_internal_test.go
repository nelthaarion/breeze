package mcp

// Tests of the notes breeze_get_timeline writes about what the dashboard can be asked, against a
// reader that answers with exactly the traffic report each case describes. They are in this
// package, and not beside the others, because the question is which sentence is chosen for which
// traffic report, and a real dashboard cannot be made to have a browser connected from a test.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/livesource"
)

func timelineNotes(t *testing.T, traffic map[string]any) string {
	t.Helper()
	reader := livesource.Func(func(path string, _ url.Values) (any, error) {
		switch path {
		case "/timeline":
			return []map[string]any{}, nil
		case "/traffic":
			if traffic == nil {
				return nil, &livesource.Error{Status: 404, Message: "an older dashboard has no traffic endpoint"}
			}
			return traffic, nil
		}
		return nil, &livesource.Error{Status: 404, Message: path}
	})
	res := getTimeline(timelineArgs{liveArgs: liveArgs{ServiceURL: thisProcess, local: reader}})
	if res.IsError {
		t.Fatalf("the list failed: %+v", res.Content)
	}
	report, ok := res.StructuredContent.(timelineListReport)
	if !ok {
		t.Fatalf("the list answered %T", res.StructuredContent)
	}
	return strings.Join(report.Notes, "\n")
}

func TestTimelineNotesSayWhetherABrowserIsConnectedAndWhyNothingIsTimed(t *testing.T) {
	counting := func(watching bool) map[string]any {
		return map[string]any{"instrumented": true, "enabled": true, "capture": map[string]any{"watching": watching}}
	}
	cases := []struct {
		name    string
		traffic map[string]any
		say     []string
		silent  []string
	}{
		{"a browser is connected", counting(true),
			[]string{"A browser is connected now"}, []string{"No browser is connected now", "middleware is not"}},
		{"nobody is watching", counting(false),
			[]string{"No browser is connected now"}, []string{"A browser is connected now", "middleware is not"}},
		{"an older dashboard that cannot say", nil,
			[]string{"a missing timeline is not evidence"}, []string{"connected now", "middleware is not"}},
		{"the middleware was never created", map[string]any{"instrumented": false, "enabled": true, "capture": map[string]any{"watching": true}},
			[]string{"dashboard.Middleware(coll) was never created", "whoever has the dashboard open"}, []string{"connected now"}},
		{"the dashboard is disabled", map[string]any{"instrumented": true, "enabled": false, "capture": map[string]any{"watching": true}},
			[]string{"Config.Enabled is false", "whoever has the dashboard open"}, []string{"connected now", "middleware is not"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notes := timelineNotes(t, tc.traffic)
			for _, want := range tc.say {
				if !strings.Contains(notes, want) {
					t.Errorf("the notes do not say %q: %s", want, notes)
				}
			}
			for _, not := range tc.silent {
				if strings.Contains(notes, not) {
					t.Errorf("the notes say %q, which is not true of this dashboard: %s", not, notes)
				}
			}
		})
	}
}
