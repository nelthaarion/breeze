package generator

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestLoggingBlockFeedsTheDashboardOnlyWhenThereIsOne: LoggingMiddleware writes to stdout and nowhere
// else, so a generated service had a dashboard whose http log tab was empty however many requests it
// served, and an MCP tool, breeze_get_logs, that read it and found nothing. With a dashboard the block
// writes each line to both; without one it is the plain middleware it was.
func TestLoggingBlockFeedsTheDashboardOnlyWhenThereIsOne(t *testing.T) {
	plain := generateFeature(t, features["logging"], featureCtx{})
	if strings.Contains(plain.Body, "PushLog") || strings.Contains(plain.Body, "requestLog") {
		t.Errorf("a project with no dashboard got a writer that reaches for one:\n%s", plain.Body)
	}
	if !strings.Contains(plain.Body, "router.Use(middleware.LoggingMiddleware())") {
		t.Errorf("the plain block is not the plain middleware:\n%s", plain.Body)
	}

	with := generateFeature(t, features["logging"], featureCtx{HasDashboard: true})
	for _, want := range []string{
		"LoggingMiddlewareWith(middleware.LoggingOptions{Output: requestLog{}})",
		`c.PushLog("http"`,
		"os.Stdout.Write(p)",
		// Looked up per line: setupLogging runs before setupDashboard, so at build time it is nil.
		"if c := DashboardCollector; c != nil",
	} {
		if !strings.Contains(with.Body, want) {
			t.Errorf("the block for a project with a dashboard lacks %q:\n%s", want, with.Body)
		}
	}
	if strings.Contains(with.Body, "LoggingOptions{Output: requestLog{}, ") || strings.Contains(with.Body, "Buffered") {
		t.Errorf("the block batches the lines, which loses them when the process dies:\n%s", with.Body)
	}
	if got := strings.Join(with.Imports, " "); !strings.Contains(got, `"os"`) || !strings.Contains(got, `"strings"`) {
		t.Errorf("the block uses os and strings and imports %q", got)
	}
	if len(with.Notes) != 1 || !strings.Contains(with.Notes[0], "breeze_get_logs") {
		t.Errorf("the add does not say where the lines went: %v", with.Notes)
	}

	// The body is Go: parsed as a file, it has the setup function and the writer's method.
	src := "package main\n\nimport (\n\t\"os\"\n\t\"strings\"\n\n\t\"github.com/nelthaarion/breeze/v2\"\n\tmiddleware \"github.com/nelthaarion/breeze/v2/middlewares\"\n)\n\nvar DashboardCollector *struct{ PushLog func(string, string, string) }\n\n" + with.Body + "\n"
	if _, err := parser.ParseFile(token.NewFileSet(), "logging.go", src, 0); err != nil {
		t.Errorf("the block does not parse: %v\n%s", err, src)
	}
}
