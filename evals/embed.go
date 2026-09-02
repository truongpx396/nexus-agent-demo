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
// *.yaml files directly. LoadProviderScriptCases only ever reads the
// *.yaml files directly under the root it's given (fs.ReadDir skips
// subdirectories), so corpus/heldout/ is never accidentally loaded as part
// of the visible corpus — HeldOutCorpus below is the only sanctioned way to
// reach it.
func Corpus() (fs.FS, error) {
	return fs.Sub(CorpusFS, "corpus")
}

// HeldOutCorpus returns the held-out suite (README task 10.6): graders kept
// "outside the agent's reach" — same shape as the visible corpus, same
// LoadProviderScriptCases loader, different files, never surfaced by any
// tool or catalog a running agent could read.
func HeldOutCorpus() (fs.FS, error) {
	return fs.Sub(CorpusFS, "corpus/heldout")
}
