package breeze

import (
	"math/rand"
	"strings"
	"testing"
)

func refValidHTTPToken(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)):
		default:
			return false
		}
	}
	return true
}

func TestValidHTTPTokenTableMatchesReference(t *testing.T) {
	for c := 0; c < 256; c++ {
		if got, want := validHTTPToken([]byte{byte(c)}), refValidHTTPToken([]byte{byte(c)}); got != want {
			t.Fatalf("byte %#x: got %v want %v", c, got, want)
		}
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(24))
		for j := range b {
			b[j] = byte(rng.Intn(256))
			if rng.Intn(4) != 0 {
				b[j] = "abcXYZ019-_.~!"[rng.Intn(14)]
			}
		}
		if got, want := validHTTPToken(b), refValidHTTPToken(b); got != want {
			t.Fatalf("%q: got %v want %v", b, got, want)
		}
	}
}
