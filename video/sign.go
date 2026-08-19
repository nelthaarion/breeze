package video

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Signed URLs let a server hand out a time-limited link to one file
// without putting an authorisation check in the media path. The signature
// covers the file name and the expiry, so a recipient can neither change
// which file they fetch nor how long the link lives.

// signaturePayload builds the exact bytes that are signed.
//
// The fields are joined with a newline, which cannot appear in either a
// cleaned path or a decimal timestamp. Concatenating without a separator
// would make ("ab", 1) and ("a", "b1") produce the same payload, so one
// valid link could be reshaped into a link for a different file.
func signaturePayload(name string, exp int64) []byte {
	var b strings.Builder
	b.Grow(len(name) + 24)
	b.WriteString(name)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(exp, 10))
	return []byte(b.String())
}

// computeSignature returns the URL-safe base64 HMAC-SHA256 of the payload.
func computeSignature(secret []byte, name string, exp int64) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(signaturePayload(name, exp))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Sign returns the query string that authorises name until it expires.
//
// name must be the path relative to the mount root, without a leading
// slash — the same form the resolver produces, so that what was signed and
// what is served cannot drift apart:
//
//	q := video.Sign(secret, "trailers/big.mp4", 15*time.Minute)
//	url := "https://host/videos/trailers/big.mp4?" + q
//
// The returned string has no leading "?" so it can be appended to a URL
// that may already carry parameters.
func Sign(secret []byte, name string, ttl time.Duration) string {
	return SignAt(secret, name, time.Now().Add(ttl))
}

// SignAt is [Sign] with an absolute expiry instead of a duration. It is
// the form tests and cache-friendly links use, because a fixed expiry
// produces a byte-identical URL on every call.
func SignAt(secret []byte, name string, expiry time.Time) string {
	name = canonicalName(name)

	exp := expiry.UTC().Unix()
	v := url.Values{}
	v.Set("exp", strconv.FormatInt(exp, 10))
	v.Set("sig", computeSignature(secret, name, exp))
	return v.Encode()
}

// verifySignature checks a request's exp and sig against name.
//
// It returns a distinct error per failure mode so the server can log why a
// link was refused, while the caller collapses all three into 403 on the
// wire.
func (m *mount) verifySignature(name, expTxt, sig string) error {
	if len(m.secret) == 0 {
		return nil // signing not enabled for this mount
	}
	if expTxt == "" || sig == "" {
		return fmt.Errorf("%w: missing exp or sig", ErrSignatureRequired)
	}
	exp, err := strconv.ParseInt(expTxt, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: unparseable exp %q", ErrInvalidSignature, expTxt)
	}

	// Verify before checking expiry. Doing it the other way round would
	// let an attacker learn whether a guessed expiry was "still valid"
	// without ever producing a correct signature.
	want := computeSignature(m.secret, name, exp)

	// hmac.Equal is constant time. A byte-by-byte comparison leaks the
	// length of the matching prefix through timing, which is enough to
	// recover a signature one character at a time.
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return fmt.Errorf("%w: for %q", ErrInvalidSignature, name)
	}
	if m.clock().UTC().Unix() > exp {
		return fmt.Errorf("%w: expired at %d", ErrSignatureExpired, exp)
	}
	return nil
}

// canonicalName reduces a name to the exact form the resolver produces, so
// that a signature made by [Sign] verifies against the name derived from
// the request.
//
// Signing an uncanonicalised name would create a mismatch class where
// "a//b.mp4" and "a/b.mp4" are the same file but different payloads, and
// links would fail for reasons the caller cannot see.
func canonicalName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}
