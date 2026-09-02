The release gate (README.md §9, docs/constitution.md Principle IX): it
shipped before the first behavior-bearing slice, not after, and hardened
into the full gate in Phase 10 — nothing here got rewritten to get there,
only added to.

## Core shapes (Phase 1)

- `types.go` — `Class`/`Verdict`/`Trial`/`Report`. `Verdict` is three-valued
  so an inconclusive result can never silently resolve to a pass (FR-138).
- `provider_case.go` / `runner.go` — the fake-provider suite: grades
  `internal/provider/fake` directly, the one piece of real behavior that
  existed on day one.
- `corpus/*.yaml` — regression/capability/negative cases, one class per
  earlier example; `corpus/heldout/*.yaml` is the held-out suite (task
  10.6), same shape, never loaded as part of the visible corpus
  (`LoadProviderScriptCases` only reads the `*.yaml` files directly under
  the root it's given — `HeldOutCorpus()` in embed.go is the only
  sanctioned way to reach the subdirectory).
- `cmd/runner/` — the CLI `make eval` runs.

## The hardened gate (Phase 10)

| File | What it adds |
|---|---|
| `stats.go` | Wilson score interval (`WilsonInterval`), `Interval.Separated` — regression is interval separation, never "a trial that used to pass now fails" (task 10.2) |
| `policy.go` | Per-class thresholds (`ClassPolicy`, `DefaultPolicies`); `EvaluateCase` folds k trials into a verdict — safety/regression/negative are `ExactRequired` (any single failure fails the case outright, no statistical slack); capability alone uses the Wilson-interval-vs-threshold path |
| `environment.go` | `Environment`/`Digest()` — the `eval_environment_digest`; `CompareEnvironments` refuses to compare across it (task 10.3) |
| `permission_case.go` + `corpus_safety.go` | Mandatory HITL adversarial cases (task 10.9) graded against the REAL `internal/permissions.Chain` — consent suppression, mid-run autonomy widening, standing-scope escape — no DB, no model, pure and in-process |
| `admission_case.go` (also in `corpus_safety.go`) | Descriptor-swap-after-admission (`internal/tools.Scan`) and skill-capability-widening (`internal/tools/builtin.ActivateSkill`), the other two adversarial shapes named in the testing-strategy table |
| `judge.go` | `Judge`: pinned, cross-family, calibrated against human labels to an agreement floor before it may grade anything for real (task 10.4/10.5) — an uncalibrated or unconfigured Judge resolves every case Inconclusive, never a silent pass |
| `trajectory.go` | Tool-selection accuracy + ask-vs-guess grading over a recorded `Trajectory`, not just end state (task 10.7) |
| `efficiency.go` | `EfficiencyBand` — a candidate outside its declared band BLOCKS even when quality holds (task 10.8) |
| `heldout.go` | Visible-vs-held-out pass-rate gap (task 10.6) — a widening gap is how spec-gaming announces itself |
| `artifact.go` | `ArtifactRef` — per-skill/tool/plan/team versioned case sets (task 10.10); `Gate.RunArtifact` scopes a run to one artifact's own cases |
| `baseline.go` | `Baseline`/`CheckRegressions`/`ApplyRegressions` — `evals/testdata/baseline.json` is "last known good," committed like a golden file; `make eval-baseline` regenerates it |
| `gate.go` | `Gate`/`GateInput`/`GateResult` — composes every suite above into the task 10.11 decision: **≥90% pass AND zero regressions AND every safety case exact AND no efficiency-band violation** |

`cmd/runner/main.go` wires all of it together: loads the visible + held-out
corpus, the adversarial permission/admission cases, the trajectory/
efficiency corpus, and the (today unconfigured — no live model in CI)
judge corpus, runs `Gate.Run`, checks the result against the committed
baseline, prints a per-case table with class/verdict/interval, and exits
non-zero the moment the gate doesn't hold.

Per-artifact promotion gates: `internal/evalgate` wires a hardened
`evals.Gate` into `internal/plan.Lifecycle`'s existing `EvalGate` hook
(`internal/plan/lifecycle.go`'s own doc comment names this file as the
intended wiring point) — see `internal/evalgate/adapter.go`.
