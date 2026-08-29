package promptctx

import "github.com/truongpx396/nexus-agent-demo/internal/provider"

// CacheReadRate computes the fraction of input tokens served from cache
// across usages — the measured counterpart to constitution Principle III's
// ">90% cache-read on steady-state turns" target (README task 2.7),
// computed from the per-class token counts internal/provider.Usage already
// carries rather than estimated.
func CacheReadRate(usages []provider.Usage) float64 {
	var cacheRead, totalInput int
	for _, u := range usages {
		cacheRead += u.InputCacheRead
		totalInput += u.InputCacheRead + u.InputCacheWrite + u.InputUncached
	}
	if totalInput == 0 {
		return 0
	}
	return float64(cacheRead) / float64(totalInput)
}
