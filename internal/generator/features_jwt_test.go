package generator

// features_jwt_test.go — what the generated jwt block does about a missing secret.
//
// # Why this is asserted on the emitted source
//
// The end-to-end test compiles a generated project, so it catches a block that does
// not build. It cannot catch a block that builds and is wrong, and that is exactly
// what this block used to be: it logged
//
//	warning: JWT_ACCESS_SECRET is not set — JWTAuth will reject every token
//
// and carried on serving. Both halves were wrong. An empty string is a valid HMAC
// key, so verification does not reject every token — it accepts every token an
// attacker signs with "", carrying any claims they choose, including any role. And
// a warning on a line nobody reads is not a response to an authentication bypass.
//
// A generated project cannot be run here to observe the exit, so the properties are
// asserted on the source the generator emits.

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// buildJWT generates the jwt block with the given flags.
func buildJWT(t *testing.T, args ...string) featureOutput {
	t.Helper()

	f, ok := features["jwt"]
	if !ok {
		t.Fatal("the jwt feature is not registered")
	}

	fs := flag.NewFlagSet("add jwt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	generate := f.Build(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}

	out, err := generate(featureCtx{ModulePath: "example.com/proj"})
	if err != nil {
		t.Fatalf("generating the jwt block: %v", err)
	}
	return out
}

func TestGeneratedJWTBlockExitsWithoutASecret(t *testing.T) {
	body := buildJWT(t).Body

	if strings.Contains(body, "log.Println") {
		t.Error("the block still only warns about a missing secret; an unset secret accepts " +
			"forged tokens, so it must not start")
	}
	if !strings.Contains(body, "log.Fatal") {
		t.Fatalf("the block does not exit when the secret is unset:\n%s", body)
	}

	// The message has to name the variable, because that is the reader's next
	// action, and it has to say what an unset secret actually does — "rejects every
	// token" was the previous text and is the opposite of the truth.
	if !strings.Contains(body, "JWT_ACCESS_SECRET is not set") {
		t.Errorf("the message does not name the variable:\n%s", body)
	}
	if strings.Contains(body, "reject every token") {
		t.Errorf("the message still claims an empty secret rejects tokens:\n%s", body)
	}
}

// TestGeneratedJWTBlockChecksTheRefreshSecret covers the half a single check
// misses: the access secret is set, so the obvious guard passes, and refresh
// tokens are still verified against "".
//
// That path is worse than the access path — the middleware exchanges a valid
// refresh token for an access token it signs itself.
func TestGeneratedJWTBlockChecksTheRefreshSecret(t *testing.T) {
	body := buildJWT(t, "--refresh").Body

	if !strings.Contains(body, "JWT_ACCESS_SECRET_REFRESH") {
		t.Fatalf("the refresh secret is used but never checked:\n%s", body)
	}
	// Two exits, not one: a shared check could not name which variable is missing.
	if got := strings.Count(body, "log.Fatal"); got != 2 {
		t.Errorf("found %d log.Fatal call(s), want 2 (access and refresh):\n%s", got, body)
	}
}

// TestGeneratedJWTBlockWithoutRefreshDoesNotCheckARefreshSecret keeps the guard
// from being stricter than the vulnerability. RefreshSecret is unused when refresh
// is off, so requiring it would block a valid configuration.
func TestGeneratedJWTBlockWithoutRefreshDoesNotCheckARefreshSecret(t *testing.T) {
	body := buildJWT(t).Body

	if strings.Contains(body, "_REFRESH") {
		t.Errorf("a refresh secret is required with refresh off:\n%s", body)
	}
}

// TestGeneratedJWTNoteSaysTheAppExits — the note is what a developer reads after
// running the command, and "set this variable" does not convey that the process
// will not start without it.
func TestGeneratedJWTNoteSaysTheAppExits(t *testing.T) {
	notes := strings.Join(buildJWT(t).Notes, " ")

	if !strings.Contains(notes, "exits") {
		t.Errorf("no note says the app exits without the secret: %q", notes)
	}
}
