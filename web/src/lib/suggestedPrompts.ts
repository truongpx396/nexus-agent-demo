// Suggested prompts (docs/build-phases.md Phase 16, task 16.9): one-click
// starting points that exercise the agentic capabilities documented in
// docs/agentic-capabilities.md -- platform/web_crawl (Crawl4AI),
// NEXUS_SANDBOX=opensandbox-backed platform/shell, activate_skill, and
// platform/delegate's "web-researcher"/"sandbox-runner" scope_grant
// conventions -- without requiring anyone to type a prompt from scratch to
// try a conversation turn.

export interface SuggestedPrompt {
  id: string;
  label: string;
  text: string;
  hint: string;
}

// For NewRun: the first message of a fresh run.
export const suggestedPrompts: SuggestedPrompt[] = [
  {
    id: "web-research",
    label: "Research & summarize",
    text: "Research what Crawl4AI is used for and summarize the top 3 points, citing your sources.",
    hint: "activate_skill(\"web-research\") -> platform/web_crawl",
  },
  {
    id: "delegate-research",
    label: "Delegate to a web-researcher",
    text: "Delegate to a web-researcher sub-agent: find out what OpenSandbox is used for, and report back with citations.",
    hint: "platform/delegate(agent_id=\"web-researcher\", scope_grant=[web_crawl, web_fetch, activate_skill])",
  },
  {
    id: "sandboxed-code",
    label: "Run code in the sandbox",
    text: "Write and run a shell command that prints the first 10 Fibonacci numbers, then show me the output.",
    hint: "activate_skill(\"sandboxed-code\") -> platform/shell (NEXUS_SANDBOX=docker|opensandbox)",
  },
  {
    id: "delegate-sandbox",
    label: "Delegate to a sandbox-runner",
    text: "Delegate to a sandbox-runner sub-agent: run `python3 --version` and `uname -a`, then report the output.",
    hint: "platform/delegate(agent_id=\"sandbox-runner\", scope_grant=[shell, activate_skill])",
  },
  {
    id: "explain-permissions",
    label: "Explain the permission chain",
    text: "In plain language, explain what this platform's permission chain does and why each layer exists.",
    hint: "No tools needed -- works without Crawl4AI/OpenSandbox running",
  },
];

// For RunDetail's steer box: a shorter follow-up turn injected mid-run.
export const suggestedFollowUps: SuggestedPrompt[] = [
  {
    id: "cite-sources",
    label: "Ask for sources",
    text: "Now list every source URL you used, one per line.",
    hint: "Follow-up turn",
  },
  {
    id: "switch-to-sandbox",
    label: "Delegate to sandbox-runner",
    text: "Now delegate the code-running part to a sandbox-runner sub-agent instead of doing it inline.",
    hint: "platform/delegate(agent_id=\"sandbox-runner\", ...)",
  },
  {
    id: "try-again-crawl",
    label: "Ask it to crawl instead of fetch",
    text: "Try that again using web_crawl instead of web_fetch, so the page is rendered before extraction.",
    hint: "platform/web_crawl",
  },
];
