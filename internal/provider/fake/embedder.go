package fake

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// EmbeddingDimensions is this fake's fixed output width — see
// internal/retrieval.EmbeddingDimensions's own doc comment for why the two
// constants must stay equal and why they are declared independently rather
// than one importing the other (internal/provider/fake must not depend on
// internal/retrieval, a much later package in the dependency order).
const EmbeddingDimensions = 32

// approxCharsPerToken mirrors internal/tools/budget.go's own documented
// estimate (that file's doc comment: "this demo has no tokenizer of its
// own") — duplicated rather than imported for the same reason
// internal/tools/builtin/web_fetch.go duplicates its SSRF CIDR list: one
// small constant isn't worth a cross-package dependency.
const approxCharsPerToken = 4

// Embedder is the deterministic embedding fake README task 12.5 mandates:
// "no correctness test calls a live embedding model." It derives a fixed-
// width vector from each text's SHA-256 digest — different texts reliably
// produce different vectors, and the same text always produces the same
// one, which is everything a test of the surrounding PLUMBING (reservation,
// indexing, search ranking, erasure) needs. It has no notion of semantic
// similarity: two texts about the same topic are no more likely to land
// near each other than two unrelated ones, exactly as scripted-chunk
// Provider replies have no notion of being a good ANSWER. Real semantic
// quality is a real embedding model's job, never this package's.
type Embedder struct{}

func NewEmbedder() *Embedder { return &Embedder{} }

// Embed hashes each text into EmbeddingDimensions float32s and L2-normalizes
// the result, so every returned vector has unit length — the same
// normalization a real embedding API's output typically has, which keeps a
// cosine-distance search (internal/retrieval's own `<=>` operator) working
// on a fake vector exactly the way it would on a real one.
func (e *Embedder) Embed(_ context.Context, texts []string, _ provider.RunContext) ([]provider.Embedding, provider.EmbedUsage, error) {
	out := make([]provider.Embedding, len(texts))
	totalChars := 0
	for i, text := range texts {
		out[i] = embedOne(text)
		totalChars += len(text)
	}
	tokens := totalChars / approxCharsPerToken
	if tokens == 0 && totalChars > 0 {
		tokens = 1
	}
	return out, provider.EmbedUsage{Tokens: tokens}, nil
}

// embedOne expands one SHA-256 digest into EmbeddingDimensions*4 bytes by
// re-hashing the running digest (digest, hash(digest), hash(hash(digest)),
// ...) — a plain, non-cryptographic stream-of-hash-blocks construction, not
// a security primitive; SHA-256 is used here only because it is a
// convenient, already-imported, deterministic 32-byte mixing function
// (crypto/rand would be the wrong tool: task 12.5 needs REPRODUCIBLE
// vectors, and .golangci.yml's forbidigo rule bans math/rand outside a
// security decision this isn't one of either — hashing sidesteps both).
func embedOne(text string) provider.Embedding {
	vec := make(provider.Embedding, EmbeddingDimensions)
	block := sha256.Sum256([]byte(text))
	bytesPerDim := 4
	needed := EmbeddingDimensions * bytesPerDim
	buf := make([]byte, 0, needed+len(block))
	for len(buf) < needed {
		buf = append(buf, block[:]...)
		block = sha256.Sum256(block[:])
	}
	var sumSquares float64
	for i := 0; i < EmbeddingDimensions; i++ {
		bits := binary.BigEndian.Uint32(buf[i*bytesPerDim : (i+1)*bytesPerDim])
		// Map the full uint32 range onto [-1, 1] before normalizing, so no
		// dimension is degenerately biased toward one sign.
		v := float32(int64(bits)-1<<31) / float32(1<<31)
		vec[i] = v
		sumSquares += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSquares)
	if norm == 0 {
		vec[0] = 1 // the astronomically unlikely all-zero hash: still return a valid unit vector
		return vec
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}
