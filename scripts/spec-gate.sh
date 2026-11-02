#!/usr/bin/env bash
# Spec-first gate: a change to behaviour-bearing code must come with a change under specs/.
#
# The specs are the single source of truth for what is built and what is open (specs/README.md). A hand-kept tracker drifts unless something makes
# updating it part of the change, so this is that something. It checks that a diff which touches behaviour also touches a spec, not that the spec
# is right: whether it is true is what /speckit-analyze and /speckit-converge are for.
#
# Shared by:
#   - .github/workflows/spec-gate.yml   (the enforcing gate)
#   - `make spec-gate`                  (check before you push)
#
# Usage: scripts/spec-gate.sh [BASE_REF]      default: origin/main, else main
# Exit:  0 ok | 1 behaviour changed with no spec change | 2 could not measure
#
# Behaviour-bearing: non-test Go under internal/, cmd/, kernel/ and evals/; SQL migrations; the eval corpus; the web app's source. Docs, CI, deploy
# config, tests, lockfiles and the specs themselves are not. A change that genuinely needs no spec (a comment-only edit, a rename the specs already
# describe) carries the `no-spec` label and says why in the PR description.
set -euo pipefail

base="${1:-}"
if [ -z "$base" ]; then
  if git rev-parse --verify --quiet origin/main >/dev/null; then
    base=origin/main
  elif git rev-parse --verify --quiet main >/dev/null; then
    base=main
  else
    echo "spec-gate: no origin/main or main to compare against; pass a base ref" >&2
    exit 2
  fi
fi

if ! changed=$(git diff --name-only "${base}...HEAD"); then
  echo "spec-gate: git diff against ${base} failed (shallow clone? unknown ref?)" >&2
  exit 2
fi

code=$(grep -E '^(internal|cmd|kernel|evals)/.*\.go$|^migrations/.*\.sql$|^evals/corpus/.*\.ya?ml$|^web/src/' <<<"$changed" | grep -v '_test\.go$' || true)
specs=$(grep -E '^specs/' <<<"$changed" || true)

if [ -z "$code" ]; then
  echo "spec-gate: no behaviour-bearing code changed vs ${base}"
  exit 0
fi

n_code=$(wc -l <<<"$code" | tr -d ' ')
if [ -n "$specs" ]; then
  n_specs=$(wc -l <<<"$specs" | tr -d ' ')
  echo "spec-gate: ${n_code} code file(s) and ${n_specs} spec file(s) changed vs ${base}"
  exit 0
fi

echo "spec-gate: ${n_code} behaviour-bearing file(s) changed vs ${base} and nothing under specs/ did:" >&2
sed 's/^/  /' <<<"$code" | head -12 >&2
[ "$n_code" -gt 12 ] && echo "  ... and $((n_code - 12)) more" >&2
echo "Add or check off a task in the relevant spec (specs/README.md says which), or start one with /speckit-specify." >&2
exit 1
