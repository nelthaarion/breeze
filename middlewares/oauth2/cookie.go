package oauth2

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"

	"github.com/nelthaarion/breeze/v2"
)

// sign returns BASE64URL(HMAC-SHA256(secret, msg)). Used to authenticate cookie
// payloads (state, PKCE verifier, session data).
func sign(secret, msg string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signedValue returns "payload.signature" where signature authenticates the
// base64url-encoded payload.
func signedValue(secret, payload string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return enc + "." + sign(secret, enc)
}

// unsignValue verifies and decodes a value produced by signedValue. It uses a
// constant-time comparison to prevent signature timing attacks.
func unsignValue(secret, value string) (string, bool) {
	dot := strings.LastIndexByte(value, '.')
	if dot <= 0 || dot == len(value)-1 {
		return "", false
	}
	enc, sig := value[:dot], value[dot+1:]
	expected := sign(secret, enc)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// cookieOptions describes a cookie to be serialized into a Set-Cookie header.
type cookieOptions struct {
	Name     string
	Value    string
	Path     string
	MaxAge   int // seconds; 0 = session cookie; <0 = delete
	Secure   bool
	HTTPOnly bool
	SameSite string // "Lax", "Strict", "None"
}

// build serializes the cookie into a Set-Cookie header value. Security
// attributes (HttpOnly, Secure, SameSite) are always emitted so there is no way
// to accidentally create an insecure cookie.
func (o cookieOptions) build() string {
	var b strings.Builder
	b.WriteString(o.Name)
	b.WriteByte('=')
	b.WriteString(o.Value)

	path := o.Path
	if path == "" {
		path = "/"
	}
	b.WriteString("; Path=")
	b.WriteString(path)

	switch {
	case o.MaxAge > 0:
		b.WriteString("; Max-Age=")
		b.WriteString(strconv.Itoa(o.MaxAge))
	case o.MaxAge < 0:
		b.WriteString("; Max-Age=0")
	}

	if o.HTTPOnly {
		b.WriteString("; HttpOnly")
	}
	if o.Secure {
		b.WriteString("; Secure")
	}
	ss := o.SameSite
	if ss == "" {
		ss = "Lax"
	}
	b.WriteString("; SameSite=")
	b.WriteString(ss)
	return b.String()
}

// setCookie writes one Set-Cookie header to the response.
//
// It is called once per cookie and does no joining of its own. Breeze's response
// header map is single-valued, but its SetHeader appends when the key is already
// present and the serializer writes each stored cookie as its own header line, so
// one call per cookie is what produces two correct lines.
//
// This used to pre-join with a literal "\r\nSet-Cookie: " prefix, which predates
// that support. Once the framework grew its own separator the prefix became a
// second copy of the header name, and the response went out as
//
//	Set-Cookie: <flow-state>=; Path=/; Max-Age=0; ...
//	Set-Cookie: Set-Cookie: <session>=...; HttpOnly; SameSite=Lax
//
// whose second line browsers discard — the parsed name is "Set-Cookie: <session>",
// not a valid cookie-name — so the session cookie was never set and logging in
// silently produced no session. Breeze also refuses a value containing CR/LF, so
// the pre-joined value would have been rejected outright rather than transmitted.
func setCookie(ctx *breeze.Context, o cookieOptions) {
	ctx.SetHeader("Set-Cookie", o.build())
}

// readCookie extracts a cookie value from the request's Cookie header. Breeze
// lowercases request header keys (see request.go), so we read "cookie".
func readCookie(ctx *breeze.Context, name string) (string, bool) {
	header := ctx.Req.Header["cookie"]
	if header == "" {
		return "", false
	}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			if part[:eq] == name {
				return part[eq+1:], true
			}
		}
	}
	return "", false
}

// deleteCookie writes an expired cookie so the browser drops it.
func deleteCookie(ctx *breeze.Context, name, path string, secure bool) {
	setCookie(ctx, cookieOptions{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		Secure:   secure,
		HTTPOnly: true,
		SameSite: "Lax",
	})
}

// pathFromURL extracts the path component of an absolute or relative URL. It
// returns "" when the input has no usable path.
func pathFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Path == "" {
		return ""
	}
	return u.Path
}
