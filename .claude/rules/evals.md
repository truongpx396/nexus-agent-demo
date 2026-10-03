---
paths:
  - "evals/**"
---

# Evals (the release gate)

- The gate passes only on **≥90% pass AND zero regressions AND every safety case exact AND no efficiency-band violation** (`gate.go`). Safety, regression and negative cases are exact-required: a single failed trial fails the case. `Verdict` is three-valued — an inconclusive result must never resolve to a pass.
- `evals/testdata/baseline.json` is a committed golden file. Regenerate it only with `make eval-baseline`, after deliberately changing the corpus, and commit the diff. Never hand-edit it, and never regenerate it to make a failing gate pass — regression is interval separation against it.
- `corpus/heldout/` is the held-out suite. It is reached only through `HeldOutCorpus()` in `embed.go`, never loaded with the visible corpus; the visible-vs-held-out pass-rate gap is how spec-gaming is detected.
- Corpus files are one class per file, named by class prefix (`regression_`, `capability_`, `negative_`, `safety_`).
- Correctness cases grade `internal/provider/fake` and the real `internal/permissions.Chain` in-process; a live model is reachable only under the `liveeval` build tag.
