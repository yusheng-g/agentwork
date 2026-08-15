package mcp

import (
	"strconv"
	"strings"
	"time"
)

// toInt64 coerces an LLM argument (JSON float64/string/nil) into an int64.
// Returns false for nil or unparseable input. JSON unmarshal never produces
// custom Stringer types, so only the native JSON types are handled.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, false
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case string:
		s, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, false
		}
		return s, true
	default:
		return 0, false
	}
}

// parseBudget coerces a timeout value from the LLM into a time.Duration.
// A non-positive value means pure async (return the terminal id immediately).
// Unparseable values fall back to the default.
func parseBudget(v any, fallback time.Duration) time.Duration {
	n, ok := toInt64(v)
	if !ok {
		return fallback
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// parseInt64Ptr coerces an optional integer argument from the LLM into a
// *int64. Returns nil for nil/unparseable input (the caller's default applies).
func parseInt64Ptr(v any) *int64 {
	n, ok := toInt64(v)
	if !ok {
		return nil
	}
	return &n
}
