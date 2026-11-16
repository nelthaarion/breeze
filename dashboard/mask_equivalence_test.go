package dashboard

// maskLine got a fast path for lines with nothing to mask. These hold it to what it replaced: the
// original function is kept here as the reference, and every line of a large generated corpus has to
// come out of the new one exactly as it came out of that one.

import (
	"math/rand"
	"strings"
	"testing"
)

// maskLineReference is maskLine as it was before the fast path.
func maskLineReference(cfg Config, s string) string {
	if s == "" {
		return s
	}
	sensitive := make(map[string]struct{}, len(cfg.MaskedHeaders))
	for _, h := range cfg.MaskedHeaders {
		sensitive[strings.ToLower(h)] = struct{}{}
	}
	parts := strings.Fields(s)
	for i, p := range parts {
		key, sep, ok := sensitiveKeyIn(p, sensitive)
		if !ok {
			continue
		}
		head, _ := splitAfterValueStart(p, key, sep)
		parts[i] = head + "••••••" + trailingPunct(p, sep)
	}
	return strings.Join(parts, " ")
}

func TestMaskLineMatchesTheOriginalOnAGeneratedCorpus(t *testing.T) {
	cfg := DefaultConfig()
	// The premise: no default key is a prefix of another, or the original, which walks a map, would not
	// be deterministic and there would be nothing to hold the new one to.
	for _, a := range cfg.MaskedHeaders {
		for _, b := range cfg.MaskedHeaders {
			if a != b && strings.HasPrefix(b, a) {
				t.Fatalf("%q is a prefix of %q: the reference is order-dependent", a, b)
			}
		}
	}

	tokens := []string{
		// ordinary request-line tokens, as LoggingMiddleware writes them
		"[Breeze][2026-11-15T15:14:06Z]", "GET", "POST", "/api/orders/42", "->", "200", "(24.067µs)", "(1.5s)", "(8.388µs)",
		// secrets, in the shapes the function knows
		"token=abc123", "Token=abc123", "TOKEN:abc", "password=hunter2", "Password:", "secret=s3cr3t", "api-key=k", "X-Api-Key:abc",
		`"authorization":"Bearer-xyz"`, `{"token":"abc"}`, `"cookie":"a=b"`, "Authorization:", "authorization=Bearer", "x-auth-token=t",
		"set-cookie=a;", "cookie=a", `[token="abc"]`, "(password=x)", "“token=quoted”",
		// near misses
		"tokens=3", "mytoken=abc", "notoken", "key=value", "a:b", "12:00:01", "http://host:3000/x", "token", "tok=en",
		// non-ASCII, including a letter that lower-cases to an ASCII one (the Kelvin sign is a k)
		"héllo", "日本語", "toKen=abc", "pasſword=x", "µs", "İtoken=a", "tökén=abc",
		// punctuation and oddities
		"", "-", "=", ":", "::", "=:", `"`, "{", "}",
	}
	spaces := []string{" ", " ", " ", " ", "  ", "\t", "\n", " \n", "\u00a0", "\u2003", "\u0085"}

	rng := rand.New(rand.NewSource(1))
	skipped, masked := 0, 0
	for n := 0; n < 40000; n++ {
		var b strings.Builder
		if rng.Intn(12) == 0 {
			b.WriteString(spaces[rng.Intn(len(spaces))]) // a leading space
		}
		count := rng.Intn(8)
		for i := 0; i < count; i++ {
			if i > 0 {
				// mostly one space, sometimes something the join would change
				if rng.Intn(10) == 0 {
					b.WriteString(spaces[rng.Intn(len(spaces))])
				} else {
					b.WriteByte(' ')
				}
			}
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		if rng.Intn(12) == 0 {
			b.WriteString(spaces[rng.Intn(len(spaces))]) // a trailing space
		}
		line := b.String()

		want := maskLineReference(cfg, line)
		got := maskLine(cfg, line)
		if got != want {
			t.Fatalf("line %q: maskLine = %q, the original made %q", line, got, want)
		}
		if got == line {
			skipped++
		}
		if strings.Contains(got, "••••••") {
			masked++
		}
	}
	// The premise: the corpus has lines of both kinds in quantity, or agreeing on one proves nothing.
	if skipped < 2000 || masked < 2000 {
		t.Fatalf("the corpus has %d lines left as they were and %d masked: too lopsided to test either path", skipped, masked)
	}
}

// TestMaskLineStillNormalisesWhitespaceAndMasks pins down the two behaviours the fast path must not lose.
func TestMaskLineStillNormalisesWhitespaceAndMasks(t *testing.T) {
	cfg := DefaultConfig()
	if got := maskLine(cfg, "  GET   /x\t-> 200 \n"); got != "GET /x -> 200" {
		t.Errorf("whitespace is no longer normalised: %q", got)
	}
	if got := maskLine(cfg, "GET /x token=abc123 -> 200"); got != "GET /x token=•••••• -> 200" {
		t.Errorf("a secret is no longer masked: %q", got)
	}
	if got := maskLine(cfg, "toKen=abc"); strings.Contains(got, "abc") {
		t.Errorf("a key spelled with the Kelvin sign got past the check: %q", got)
	}
}

// TestACollectorBuiltWithoutNewCollectorStillMasks: the precomputed keys are an optimisation, and a
// Collector that never had them must behave as it always did.
func TestACollectorBuiltWithoutNewCollectorStillMasks(t *testing.T) {
	c := &Collector{cfg: DefaultConfig()}
	if got := maskLineWith(c.keysForMasking(), "login password=hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("a collector with no precomputed keys leaked a password: %q", got)
	}
}
