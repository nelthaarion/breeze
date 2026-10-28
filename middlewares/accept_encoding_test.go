package middleware

import (
	"math/rand"
	"strings"
	"testing"
)

func TestParseAcceptEncodingMatchesGeneric(t *testing.T) {
	names := []string{"br", "gzip", "deflate", "*", "BR", "GZip", "identity", "compress", "x-gzip", " br ", "\tgzip", "deflate ", "", "gzip2", "g zip"}
	qs := []string{"", ";q=1", ";q=0", ";q=0.5", "; q=0.001", ";Q=0.3", ";q=1.0", ";q=abc", ";q=-1", ";q=2", ";q= 0.7 ", ";q=.5",
		";q=1e0", ";q=0x1p-1", ";q=", ";level=1;q=0.2", ";q=0.1;q=0.9", ";;q=0.4", ";", "; q = 0.2", ";q=NaN", ";q=+1", ";x=y"}
	seps := []string{",", ", ", " , ", ",,", ";", ", ,"}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 300000; i++ {
		var sb strings.Builder
		for j, n := 0, rng.Intn(6); j < n; j++ {
			if j > 0 {
				sb.WriteString(seps[rng.Intn(len(seps))])
			}
			sb.WriteString(names[rng.Intn(len(names))])
			sb.WriteString(qs[rng.Intn(len(qs))])
		}
		h := sb.String()
		switch rng.Intn(40) {
		case 0:
			h += "\u00a0" // non-ASCII: must take the generic path
		case 1:
			h = "gz\u0130p, " + h
		}
		got := parseAcceptEncoding(h)
		for _, e := range []string{"br", "gzip", "deflate"} {
			if got.of(e) != encodingQValue(h, e) {
				t.Fatalf("header %q, %s: got %v want %v", h, e, got.of(e), encodingQValue(h, e))
			}
		}
	}
}

var sinkQ acceptQ

func TestParseAcceptEncodingNoAllocs(t *testing.T) {
	if n := testing.AllocsPerRun(1000, func() { sinkQ = parseAcceptEncoding("gzip, deflate, br;q=0.8, *;q=0.1") }); n != 0 {
		t.Fatalf("allocates %v times per call", n)
	}
}

func BenchmarkAcceptEncodingGeneric(b *testing.B) {
	b.ReportAllocs()
	h := "gzip, deflate, br"
	for i := 0; i < b.N; i++ {
		for _, e := range []string{"br", "gzip", "deflate"} {
			if encodingQValue(h, e) <= 0 {
				continue
			}
			break
		}
	}
}

func BenchmarkAcceptEncodingSinglePass(b *testing.B) {
	b.ReportAllocs()
	h := "gzip, deflate, br"
	for i := 0; i < b.N; i++ {
		q := parseAcceptEncoding(h)
		for _, e := range []string{"br", "gzip", "deflate"} {
			if q.of(e) <= 0 {
				continue
			}
			break
		}
	}
}
