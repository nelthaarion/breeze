package aggregator

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"github.com/nelthaarion/breeze"
)

// readAuth is intentionally Basic Auth rather than the dashboard's cookie
// session. Aggregator read endpoints are primarily consumed by the dashboard's
// server-side proxy, which can keep these credentials off the browser entirely.
func readAuth(cfg Config) breeze.HandlerFunc {
	if !cfg.AuthEnabled() {
		return func(ctx *breeze.Context) error { return ctx.Next() }
	}
	wantUser := sha256.Sum256([]byte(cfg.Username))
	wantPass := sha256.Sum256([]byte(cfg.Password))
	return func(ctx *breeze.Context) error {
		raw := ctx.Req.Header["authorization"]
		if strings.HasPrefix(raw, "Basic ") {
			decoded, err := base64.StdEncoding.DecodeString(raw[6:])
			if err == nil {
				if split := strings.IndexByte(string(decoded), ':'); split >= 0 {
					gotUser := sha256.Sum256(decoded[:split])
					gotPass := sha256.Sum256(decoded[split+1:])
					if subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) == 1 &&
						subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) == 1 {
						return ctx.Next()
					}
				}
			}
		}
		ctx.Res = &breeze.HTTPResponse{Status: 401, Headers: map[string]string{
			"Content-Type":     "application/json",
			"WWW-Authenticate": `Basic realm="Breeze Fleet"`,
		}, Body: []byte(`{"error":"unauthorized"}`)}
		ctx.Abort()

		return nil
	}
}

func ingestAuthorized(ctx *breeze.Context, token string) bool {
	if token == "" {
		return true
	}
	want := sha256.Sum256([]byte(token))
	got := sha256.Sum256([]byte(ctx.Req.Header["x-fleet-token"]))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
