#!/usr/bin/env bash
# PostToolUse hook (matcher: Write|Edit). For an edited .go file:
#   1. goimports -w  — gofmt + import pruning, the formatter CI lints for.
#   2. size nudge    — tell Claude when a non-test file has crossed the line
#                      cap, so it splits the file now instead of failing
#                      tests/contract/filesize_test.go later.
# Never blocks: formatting problems and a missing goimports are silent no-ops.
# Reads the hook payload (JSON) on stdin.
set -uo pipefail

file=$(jq -r '.tool_input.file_path // empty')
case "$file" in *.go) ;; *) exit 0 ;; esac
[ -f "$file" ] || exit 0

goimports=$(command -v goimports || true)
if [ -z "$goimports" ] && [ -x "$HOME/go/bin/goimports" ]; then
  goimports="$HOME/go/bin/goimports"
fi
[ -n "$goimports" ] && "$goimports" -w "$file" 2>/dev/null

# Size nudge: test files and generated files are exempt, same as the contract
# test. The cap is read from the test itself so there is one number to change.
case "$file" in *_test.go) exit 0 ;; esac
head -n 20 "$file" | grep -qE '^// Code generated .* DO NOT EDIT\.$' && exit 0

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cap=$(sed -nE 's/^const maxGoFileLines = ([0-9]+).*/\1/p' "$root/tests/contract/filesize_test.go" 2>/dev/null)
cap="${cap:-500}"

lines=$(wc -l <"$file" | tr -d ' ')
[ "$lines" -gt "$cap" ] || exit 0

msg="${file#"$root"/} is now ${lines} lines; the cap for non-test Go files is ${cap} (enforced by tests/contract/filesize_test.go). Split it by responsibility within the same package (see CLAUDE.md Conventions) before moving on."
jq -n --arg m "$msg" '{hookSpecificOutput: {hookEventName: "PostToolUse", additionalContext: $m}}'
