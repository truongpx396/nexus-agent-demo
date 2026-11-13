package promptctx

import "github.com/truongpx396/nexus-agent-demo/internal/provider"

// bytesPerEstimatedToken is how many bytes of prompt EstimateTokens counts as one
// token. English text averages about four bytes a token and code or JSON fewer, so
// three overstates, on purpose: it sizes a reservation that must not be exceeded, and
// a ceiling is better refused a little early than overshot.
const bytesPerEstimatedToken = 3

// EstimateTokens estimates the input tokens of a call that sends system, catalog and
// transcript: the bytes of the system prompt, of each tool's name, description and
// input schema, and of every block's text, tool name and input, divided by three,
// rounded up. This codebase has no tokenizer (condense.go's byte threshold and
// internal/promptctx/cache.go's reported usage are the same kind of proxy), so the
// figure is deliberately high rather than exact. It feeds the cost gate's worst-case
// reservation (cost.ReserveRequest.InputTokenEstimate); it is never what a run is
// billed, which comes from the provider's reported usage.
func EstimateTokens(system string, catalog []provider.ToolSchema, transcript []provider.Message) int64 {
	n := int64(len(system))
	for _, t := range catalog {
		n += int64(len(t.Name) + len(t.Description) + len(t.InputSchema))
	}
	for _, m := range transcript {
		for _, b := range m.Blocks {
			n += int64(len(b.Text) + len(b.ToolName) + len(b.Input))
		}
	}
	return (n + bytesPerEstimatedToken - 1) / bytesPerEstimatedToken
}
