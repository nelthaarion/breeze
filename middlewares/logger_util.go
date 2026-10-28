package middleware

import "strconv"

func appendBigInt(b []byte, n int) []byte { return strconv.AppendInt(b, int64(n), 10) }
