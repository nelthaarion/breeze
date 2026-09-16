package oauth2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// sealSession encrypts authenticated session material before it is placed in a
// browser cookie. HMAC signing prevents tampering, but it does not prevent a
// browser extension, proxy, log collector, or user inspecting the cookie from
// reading the OAuth token embedded in it. AES-GCM provides confidentiality and
// integrity without introducing server-side session state.
func sealSession(secret, plaintext string) (string, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openSession(secret, encoded string) (string, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("session cookie is too short")
	}
	returnPlain := sealed[:gcm.NonceSize()]
	plaintext, err := gcm.Open(nil, returnPlain, sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("session cookie authentication failed")
	}
	return string(plaintext), nil
}
