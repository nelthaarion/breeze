package video

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"
)

// Opaque links carry the file name *inside* the URL rather than as part of
// the path, encrypted under the mount's secret.
//
// A signed URL proves a link was issued by this server, but it still shows
// the world what it points at:
//
//	/videos/interviews/board-2026-q1.mp4?exp=...&sig=...
//
// The signature stops the recipient fetching a *different* file, and does
// nothing about the fact that the name itself is a disclosure: it reveals
// the library's layout, it invites guessing at neighbouring names, and it
// leaks into referrer headers, proxy logs, and bookmarks. An opaque link
// carries no name at all:
//
//	/videos/tqXQ0k9m3Rl2Zx8Q...
//
// The token is AES-256-GCM, so the name is confidential (encryption) and
// unforgeable (the GCM tag), and the expiry travels inside the sealed
// plaintext where it cannot be edited. That subsumes exp/sig entirely,
// which is why opaque mode does not also require a signature.

// tokenVersion prefixes every token so the format can change without
// silently misreading old links. A token that arrives with an unknown
// version is refused rather than decrypted as though it were current.
const tokenVersion byte = 1

// tokenKey derives the AES key from the mount secret.
//
// The secret is an arbitrary-length byte string chosen by the caller, and
// AES needs exactly 32 bytes, so it is hashed rather than truncated: a
// short secret must not silently become a shorter key.
//
// The domain separator matters. The same secret also produces exp/sig
// signatures, and deriving both from the raw secret would mean one
// construction's output could be fed into the other. Hashing with a
// distinct label keeps the two key spaces disjoint.
func tokenKey(secret []byte) [32]byte {
	return sha256.Sum256(append([]byte("breeze/video/token/v1\n"), secret...))
}

// tokenNonce derives the GCM nonce from the plaintext instead of taking it
// from a random source.
//
// GCM requires that a nonce never repeat under the same key. A random
// nonce satisfies that, but it also makes every call produce a different
// URL for the same file, which defeats HTTP caching: a page reload would
// issue a fresh URL, and the browser would re-download a video it already
// had. Deriving the nonce from (name, exp) makes the token deterministic,
// so a given file and expiry always yield the same cache-friendly link.
//
// Uniqueness still holds. Two tokens share a nonce only when they share a
// name and an expiry, and in that case the plaintext is identical too, so
// the ciphertext is a legitimate repeat rather than a nonce reuse across
// different messages — which is the case GCM actually forbids.
func tokenNonce(secret []byte, name string, exp int64) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("breeze/video/nonce/v1\n"))
	mac.Write(signaturePayload(name, exp))
	return mac.Sum(nil)[:12]
}

// newTokenAEAD builds the AEAD for a secret.
func newTokenAEAD(secret []byte) (cipher.AEAD, error) {
	key := tokenKey(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Token returns an opaque path segment that grants access to name until
// ttl elapses.
//
// The result replaces the file name in the URL rather than being appended
// to it:
//
//	tok := video.Token(secret, "interviews/board.mp4", 10*time.Minute)
//	url := "https://host/videos/" + tok
//
// It is URL-safe and contains no "/", so it always occupies exactly one
// path segment regardless of how deeply nested the real file is.
func Token(secret []byte, name string, ttl time.Duration) (string, error) {
	return TokenAt(secret, name, time.Now().Add(ttl))
}

// TokenAt is [Token] with an absolute expiry.
//
// Like [SignAt], it is the form to use when a byte-identical URL matters:
// rounding the expiry to a fixed boundary (the top of the hour, say) makes
// the token stable for that whole window, so browsers and CDNs can cache
// it instead of re-fetching on every page load.
func TokenAt(secret []byte, name string, expiry time.Time) (string, error) {
	if len(secret) == 0 {
		return "", fmt.Errorf("%w: a secret is required to issue tokens", ErrInvalidConfig)
	}
	aead, err := newTokenAEAD(secret)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	// Canonicalise before sealing, so the name inside the token is
	// exactly what the resolver will produce from it. Sealing a raw name
	// would create tokens that decrypt to a path the mount then refuses.
	name = canonicalName(name)
	exp := expiry.UTC().Unix()

	plain := make([]byte, 8, 8+len(name))
	binary.BigEndian.PutUint64(plain, uint64(exp))
	plain = append(plain, name...)

	nonce := tokenNonce(secret, name, exp)

	// The version byte is authenticated as additional data rather than
	// merely prepended: that way a token cannot be replayed as a
	// different version by editing the prefix.
	sealed := aead.Seal(nil, nonce, plain, []byte{tokenVersion})

	out := make([]byte, 0, 1+len(nonce)+len(sealed))
	out = append(out, tokenVersion)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// parseToken opens a token and returns the name it authorises.
//
// Every failure is reported as a signature error so the caller maps it to
// 403 exactly as it does for exp/sig. The distinctions are kept for the
// log, where "expired" and "forged" call for very different reactions.
func (m *mount) parseToken(tok string) (string, error) {
	if tok == "" {
		return "", fmt.Errorf("%w: empty token", ErrSignatureRequired)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", fmt.Errorf("%w: token is not valid base64url", ErrInvalidSignature)
	}
	if len(raw) < 1+12+16 { // version + nonce + GCM tag
		return "", fmt.Errorf("%w: token is too short", ErrInvalidSignature)
	}
	if raw[0] != tokenVersion {
		return "", fmt.Errorf("%w: unsupported token version %d", ErrInvalidSignature, raw[0])
	}

	aead, err := newTokenAEAD(m.secret)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}

	nonce, body := raw[1:13], raw[13:]

	// Open both decrypts and authenticates: a token with a single edited
	// byte fails here rather than yielding a plausible name.
	plain, err := aead.Open(nil, nonce, body, []byte{tokenVersion})
	if err != nil {
		return "", fmt.Errorf("%w: token failed authentication", ErrInvalidSignature)
	}
	if len(plain) < 9 {
		return "", fmt.Errorf("%w: token carries no name", ErrInvalidSignature)
	}

	exp := int64(binary.BigEndian.Uint64(plain[:8]))
	name := string(plain[8:])

	// The nonce is derived from the plaintext, so a token whose nonce
	// does not match what its own contents imply was assembled by hand.
	// GCM would not catch this: the attacker controls the nonce field.
	if !hmac.Equal(nonce, tokenNonce(m.secret, name, exp)) {
		return "", fmt.Errorf("%w: token nonce does not match its contents", ErrInvalidSignature)
	}

	// Expiry is checked after authentication, so an unauthenticated
	// caller cannot probe which expiries are still live.
	if m.clock().UTC().Unix() > exp {
		return "", fmt.Errorf("%w: token expired at %d", ErrSignatureExpired, exp)
	}
	return name, nil
}
