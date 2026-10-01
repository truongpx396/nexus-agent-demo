#!/usr/bin/env bash
# PreToolUse hook (matcher: Bash, filtered to `gh pr create`). Advisory:
# measures the branch with scripts/pr-size.sh and, if it is over the limit,
# tells Claude and the user BEFORE the PR is opened — splitting is cheap now
# and expensive after review starts. It never blocks; the real gate is
# .github/workflows/pr-size.yml.
# Reads the hook payload (JSON) on stdin.
set -uo pipefail

cmd=$(jq -r '.tool_input.command // empty')
case "$cmd" in *"gh pr create"*) ;; *) exit 0 ;; esac

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$root" || exit 0

out=$("$root/scripts/pr-size.sh" 2>&1)
rc=$?
# 0 = within limit; anything but 1 = could not measure. Stay quiet for both:
# an advisory hook must never nag about its own failure.
[ "$rc" -eq 1 ] || exit 0

msg="PR size: ${out}. CI's pr-size check fails above the limit unless the PR has the 'large-pr' label. Consider splitting this into smaller PRs first; if it is a mechanical change (rename, file split) that cannot be split, open it, add the 'large-pr' label, and say why in the description."
jq -n --arg m "$msg" '{systemMessage: $m, hookSpecificOutput: {hookEventName: "PreToolUse", additionalContext: $m}}'
