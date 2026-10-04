package httpx

import (
	"context"
	"net/http"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func contextWith(r *http.Request, key ctxKey, v any) context.Context {
	return context.WithValue(r.Context(), key, v)
}

// Query returns the first value of a query parameter and whether it was present.
func Query(r *http.Request, key string) (string, bool) {
	vals, ok := r.URL.Query()[key]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

// QueryInt parses a query parameter the way the Nest handlers' `int()` helper does: Number(v) truncated, else fallback.
func QueryInt(r *http.Request, key string, fallback int) int {
	raw, ok := Query(r, key)
	if !ok {
		return fallback
	}
	if raw == "" {
		return 0 // Number("") is 0 in JavaScript
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f != f || f > 1e15 || f < -1e15 {
		return fallback
	}
	return int(f)
}
