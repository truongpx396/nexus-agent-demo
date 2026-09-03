package retrieval

import (
	"fmt"
	"strconv"
	"strings"
)

// formatVector renders a []float32 in pgvector's own text input format —
// "[v1,v2,v3]" — passed as a plain string parameter cast with `::vector` in
// SQL. This demo has no reason to pull in the pgvector-go driver module
// just to register one pgx type: pgvector's text format is simple, stable,
// and documented, and a plain string round-trip through ::vector is exactly
// as correct as a binary encoding for this package's read/write volume.
func formatVector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// parseVector is formatVector's inverse, used when a query selects
// `embedding::text` back out of Postgres (store.go's own SELECT casts it
// explicitly for this reason — the raw pgvector wire type has no meaning to
// pgx without the driver extension this package deliberately doesn't take
// on).
func parseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("retrieval: parse vector component %q: %w", p, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}
