package middleware

import (
	"strconv"
	"strings"
)

// acceptQ holds the quality values of the three encodings this package can
// produce, as encodingQValue reports them for the same header.
type acceptQ struct{ br, gzip, deflate float64 }

func (a acceptQ) of(encoding string) float64 {
	switch encoding {
	case "br":
		return a.br
	case "gzip":
		return a.gzip
	case "deflate":
		return a.deflate
	}
	return 0
}

// parseAcceptEncoding evaluates an Accept-Encoding header for br, gzip and
// deflate in one pass without allocating. encodingQValue, which it replaces on
// the request path, re-split the header with strings.Split for every candidate
// encoding (up to three times per request, several allocations each).
//
// The result is identical to calling encodingQValue once per encoding: the last
// explicit entry for an encoding wins, then the last "*", then 0. A header with
// non-ASCII bytes is handed to encodingQValue itself so its Unicode case folding
// and whitespace rules apply unchanged.
func parseAcceptEncoding(header string) acceptQ {
	for i := 0; i < len(header); i++ {
		if header[i] >= 0x80 {
			return acceptQ{encodingQValue(header, "br"), encodingQValue(header, "gzip"), encodingQValue(header, "deflate")}
		}
	}
	best := [3]float64{-1, -1, -1}
	wildcard := -1.0
	rest := header
	for {
		raw, more := rest, false
		if i := strings.IndexByte(rest, ','); i >= 0 {
			raw, rest, more = rest[:i], rest[i+1:], true
		}
		name, params := raw, ""
		if i := strings.IndexByte(raw, ';'); i >= 0 {
			name, params = raw[:i], raw[i+1:]
		}
		q := 1.0
		for params != "" {
			param := params
			if i := strings.IndexByte(params, ';'); i >= 0 {
				param, params = params[:i], params[i+1:]
			} else {
				params = ""
			}
			param = strings.TrimSpace(param)
			if eq := strings.IndexByte(param, '='); eq >= 0 && strings.EqualFold(strings.TrimSpace(param[:eq]), "q") {
				if v, err := strconv.ParseFloat(strings.TrimSpace(param[eq+1:]), 64); err == nil && v >= 0 && v <= 1 {
					q = v
				} else {
					q = 0
				}
			}
		}
		switch name = strings.TrimSpace(name); {
		case strings.EqualFold(name, "br"):
			best[0] = q
		case strings.EqualFold(name, "gzip"):
			best[1] = q
		case strings.EqualFold(name, "deflate"):
			best[2] = q
		case name == "*":
			wildcard = q
		}
		if !more {
			break
		}
	}
	resolve := func(v float64) float64 {
		if v >= 0 {
			return v
		}
		if wildcard >= 0 {
			return wildcard
		}
		return 0
	}
	return acceptQ{resolve(best[0]), resolve(best[1]), resolve(best[2])}
}
