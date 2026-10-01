#!/usr/bin/env bash
# PostToolUse hook (matcher: Bash, filtered to `gh pr create`). Opening a PR is
# not the end of the job: put "verify CI before you call this done" in front of
# Claude at the exact moment it is most likely to stop. Injects context only;
# it never blocks and never runs CI itself (a CI run outlasts a hook timeout).
# Reads the hook payload (JSON) on stdin.
set -uo pipefail

payload=$(cat)

cmd=$(jq -r '.tool_input.command // empty' <<<"$payload")
case "$cmd" in *"gh pr create"*) ;; *) exit 0 ;; esac

# gh prints the new PR's URL on success. Flatten every string in the tool
# response (its exact shape varies) and take the first PR URL found. No URL
# means --help, --web, or a failed create: nothing to verify, stay quiet.
output=$(jq -r '.tool_response // empty | [.. | strings] | join("\n")' <<<"$payload" 2>/dev/null)
url=$(grep -oE 'https://github\.com/[^/[:space:]"]+/[^/[:space:]"]+/pull/[0-9]+' <<<"$output" | head -n1)
[ -n "$url" ] || exit 0
num=${url##*/}

msg="PR #${num} is open (${url}). Do not report it as done yet: verify CI first. Run \`gh pr checks ${num} --watch\` (use run_in_background if you have other work) and report each check's outcome. 'skipped' is not 'passed': unit, integration and eval-gate are skipped whenever lint fails, because they need it. If a check fails, read \`gh run view <run-id> --log-failed\`, then say whether this PR caused it or main was already red (\`gh run list --branch main --workflow ci --limit 1\`). Never say CI is green unless you saw it."

jq -n --arg m "$msg" '{hookSpecificOutput: {hookEventName: "PostToolUse", additionalContext: $m}}'
