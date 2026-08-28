package evals

import (
	"embed"
	"io/fs"
)

// CorpusFS embeds the case corpus into the binary so `nexusd`-adjacent
// tooling (evals/cmd/runner) carries it without depending on the working
// directory at runtime — the same reasoning as migrations/embed.go.
//
//go:embed corpus
var CorpusFS embed.FS

// Corpus returns CorpusFS rooted at the corpus/ directory, so callers pass
// LoadProviderScriptCases a filesystem whose root already contains the
// *.yaml files directly.
func Corpus() (fs.FS, error) {
	return fs.Sub(CorpusFS, "corpus")
}
