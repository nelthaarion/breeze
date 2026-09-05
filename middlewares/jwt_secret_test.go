package middleware

// jwt_secret_test.go — the empty-secret refusal.
//
// # Why this is a test and not a doc comment
//
// HS256 accepts a zero-length key. A JWTAuthMiddleware constructed without a
// secret therefore does not reject every token, which is what one might assume
// and what the generated code used to say: it *accepts* every token an attacker
// signs with "", carrying whatever claims they chose. The forgery is trivial —
// the key is the empty string, which is public knowledge.
//
// The first test below demonstrates the forgery against the library the framework
// actually uses, so the refusal is justified by observed behaviour rather than by
// an assumption about HMAC. The rest assert the refusal.

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nelthaarion/breeze/v2"
)

// TestAnEmptySecretVerifiesAForgedToken is the vulnerability, demonstrated.
//
// If this ever fails because the JWT library started refusing empty keys, the
// panic in JWTAuthMiddleware becomes belt-and-braces rather than load-bearing —
// which is worth knowing, and is not a reason to remove it.
func TestAnEmptySecretVerifiesAForgedToken(t *testing.T) {
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "attacker",
		"role":    "admin",
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	signed, err := forged.SignedString([]byte(""))
	if err != nil {
		t.Fatalf("signing with an empty secret failed: %v", err)
	}

	claims, valid := validateJWT(signed, "", jwt.SigningMethodHS256)
	if !valid {
		t.Skip("the JWT library now refuses empty keys; the construction-time panic is " +
			"redundant but harmless")
	}
	if claims["role"] != "admin" {
		t.Fatalf("claims = %v", claims)
	}
	t.Log("an empty secret verifies an attacker-signed admin token — which is why " +
		"JWTAuthMiddleware refuses to be constructed without one")
}

func TestJWTAuthMiddlewareRefusesAnEmptyAccessSecret(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("JWTAuthMiddleware accepted an empty AccessSecret, which accepts forged tokens")
		}
		msg, _ := r.(string)
		// The message has to name the field, because the caller's next action is
		// to set it and a generic "invalid configuration" does not say which.
		if !strings.Contains(msg, "AccessSecret") {
			t.Errorf("the panic does not name the field: %v", r)
		}
	}()

	JWTAuthMiddleware(JWTOptions{})
}

// TestJWTAuthMiddlewareRefusesRefreshWithoutARefreshSecret covers the half that
// is easy to miss: an access secret is set, so the obvious check passes, and the
// refresh path still verifies against "".
//
// That path is worse than the access path, because a forged refresh token is
// exchanged for a genuine access token the middleware itself signs.
func TestJWTAuthMiddlewareRefusesRefreshWithoutARefreshSecret(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("EnableRefreshToken was accepted with no RefreshSecret")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "RefreshSecret") {
			t.Errorf("the panic does not name the field: %v", r)
		}
	}()

	JWTAuthMiddleware(JWTOptions{AccessSecret: "real-secret", EnableRefreshToken: true})
}

// TestJWTAuthMiddlewareAcceptsARealSecret is the control: the refusal must not
// have made the middleware unconstructible.
func TestJWTAuthMiddlewareAcceptsARealSecret(t *testing.T) {
	mw := JWTAuthMiddleware(JWTOptions{AccessSecret: "real-secret"})
	if mw == nil {
		t.Fatal("JWTAuthMiddleware returned nil for a valid configuration")
	}

	// And it still rejects an unsigned request, which is the behaviour the panic
	// exists to protect rather than replace.
	ctx := breeze.NewContext(breeze.GET, "/")
	reached := false
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(*breeze.Context) error {
		reached = true
		return nil
	})
	_ = ctx.Next()

	if reached {
		t.Error("a request with no token reached the handler")
	}
	if ctx.Res == nil || ctx.Res.Status != 401 {
		t.Errorf("status = %v, want 401", ctx.Res)
	}
}

// TestARefreshSecretIsOptionalWithoutRefresh keeps the check from being stricter
// than the vulnerability: RefreshSecret is unused when refresh is off.
func TestARefreshSecretIsOptionalWithoutRefresh(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a config with no RefreshSecret and refresh off was refused: %v", r)
		}
	}()
	JWTAuthMiddleware(JWTOptions{AccessSecret: "real-secret"})
}
