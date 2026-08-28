The release gate (docs/constitution.md, Principle IX): it ships before the
first behavior-bearing slice, not after.

- `types.go` — `Class`/`Verdict`/`Trial`/`Report`. `Verdict` is three-valued
  so an inconclusive result can never silently resolve to a pass (FR-138).
- `provider_case.go` / `runner.go` — Phase 1's one suite: it grades the
  deterministic fake provider itself (`internal/provider/fake`), the only
  real behavior that exists this early. Phase 2 onward adds suites that
  exercise the kernel through the same `Trial`/`Report` shapes.
- `corpus/*.yaml` — one case per suite class (regression / capability /
  safety / negative). A safety-class case (`safety_truncated_stream_is_an_error.yaml`)
  demonstrates the rule a safety case exists to enforce: a truncated stream
  must surface as an error, never a silent success.
- `cmd/runner/` — the CLI `make eval` runs: loads the corpus, grades it,
  prints a table, exits non-zero on any failure.

Phase 9 hardens this into the full gate: k trials per case with exact
intervals, a pinned `eval_environment_digest`, a calibrated cross-family
judge, trajectory grading, and efficiency gating. Nothing here gets rewritten
to get there — only added to.
