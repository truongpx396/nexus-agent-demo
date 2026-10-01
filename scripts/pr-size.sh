#!/usr/bin/env bash
# Measures how big this branch's PR is: insertions + deletions against the
# merge base with BASE_REF, excluding files that are noise rather than review
# surface (lockfiles, build output, the generated eval baseline).
#
# Single source of truth for the limit and the exclusions. Shared by:
#   - .github/workflows/pr-size.yml   (the enforcing gate)
#   - `make pr-size`                  (check before you push)
#   - .claude/hooks/pr-size-check.sh  (advisory nudge before `gh pr create`)
#
# Usage: scripts/pr-size.sh [BASE_REF]      default: origin/main, else main
# Env:   PR_SIZE_LIMIT                      max changed lines (default 1000)
# Exit:  0 within limit | 1 over limit | 2 could not measure
set -euo pipefail

limit="${PR_SIZE_LIMIT:-1000}"

base="${1:-}"
if [ -z "$base" ]; then
  if git rev-parse --verify --quiet origin/main >/dev/null; then
    base=origin/main
  elif git rev-parse --verify --quiet main >/dev/null; then
    base=main
  else
    echo "pr-size: no origin/main or main to compare against; pass a base ref" >&2
    exit 2
  fi
fi

# Pathspec excludes: files nobody reviews line by line. Keep this list short
# and mechanical; a reviewable-but-large change is what the `large-pr` label
# is for, not a new entry here.
#   - lockfiles, build output, and the generated eval baseline
#   - Spec Kit scaffolding, which `specify init`/upgrade writes wholesale.
#     (.specify/memory/constitution.md and anything hand-written under
#     .claude/ stay counted.)
excludes=(
  ':(exclude)*package-lock.json'
  ':(exclude)go.sum'
  ':(exclude)web/dist'
  ':(exclude)evals/testdata/baseline.json'
  ':(exclude).claude/skills/speckit-*'
  ':(exclude).specify/scripts'
  ':(exclude).specify/templates'
  ':(exclude).specify/integrations'
  ':(exclude).specify/workflows'
)

if ! numstat=$(git diff --numstat "${base}...HEAD" -- . "${excludes[@]}"); then
  echo "pr-size: git diff against ${base} failed (shallow clone? unknown ref?)" >&2
  exit 2
fi

# numstat prints "-" for binary files; count those as 0.
read -r added deleted < <(awk '
  $1 != "-" { a += $1 }
  $2 != "-" { d += $2 }
  END { print a + 0, d + 0 }
' <<<"$numstat")

changed=$((added + deleted))
echo "${changed} changed lines (+${added} -${deleted}) vs ${base}, limit ${limit}"

if [ "$changed" -gt "$limit" ]; then
  exit 1
fi
