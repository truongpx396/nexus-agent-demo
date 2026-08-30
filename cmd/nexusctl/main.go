// Command nexusctl is the CLI surface adapter — the second surface that
// proves Principle I (one loop, many surfaces): a thin translator over the
// REST API (internal/surfaces/rest), zero control flow of its own. A full
// surface (session listing, streaming SSE, ...) is Phase 7's; this is
// deliberately just enough to drive README §5's Phase 5 demo: start a run,
// and show/grant/deny the approval it suspends on.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("nexusctl %s (%s)\n", version.Version, version.GitCommit)
		printUsage()
		return
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "approvals":
		err = cmdApprovals(os.Args[2:])
	case "cancel":
		err = cmdCancel(os.Args[2:])
	case "steer":
		err = cmdSteer(os.Args[2:])
	case "fork":
		err = cmdFork(os.Args[2:])
	default:
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nexusctl: "+err.Error())
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`usage:
  nexusctl run "<input>" [--autonomy=read_only|supervised|autonomous] [--budget=<usd>]
  nexusctl approvals show <approval-id>
  nexusctl approvals grant <approval-id> [--modify='{"field":"value"}']
  nexusctl approvals deny <approval-id> --reason="<reason>"
  nexusctl cancel <session-id> [--reason="<reason>"]
  nexusctl steer <session-id> "<input>"
  nexusctl fork <session-id> --at=<seq> [--model=<model-id>]

env: NEXUS_HTTP_ADDR (default http://localhost:8080),
     NEXUS_TENANT_ID, NEXUS_USER_ID (dev-mode principal headers)`)
}

// client is the thin HTTP + dev-mode-principal wrapper every subcommand
// shares — this file's only "control flow" beyond argument parsing, and
// even that is just header plumbing, never a decision about what a run
// should do (Principle I: surfaces translate I/O only).
type client struct {
	baseURL  string
	tenantID string
	userID   string
}

func newClient() *client {
	return &client{
		baseURL:  envOr("NEXUS_HTTP_ADDR", "http://localhost:8080"),
		tenantID: envOr("NEXUS_TENANT_ID", ""),
		userID:   envOr("NEXUS_USER_ID", ""),
	}
}

func (c *client) do(method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader) //nolint:noctx // a short-lived CLI invocation; there is no caller context to thread through
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Nexus-Tenant-ID", c.tenantID)
	req.Header.Set("X-Nexus-User-ID", c.userID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body; nothing to flush
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return respBody, resp.StatusCode, nil
}

func parseFlags(args []string) (positional []string, flags map[string]string) {
	flags = map[string]string{}
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			positional = append(positional, a)
			continue
		}
		key, val, hasVal := strings.Cut(a[2:], "=")
		if !hasVal {
			val = "true"
		}
		flags[key] = val
	}
	return positional, flags
}

func cmdRun(args []string) error {
	positional, flags := parseFlags(args)
	if len(positional) != 1 {
		return fmt.Errorf(`run requires exactly one argument: nexusctl run "<input>"`)
	}

	body := map[string]string{"input": positional[0]}
	if v, ok := flags["autonomy"]; ok {
		body["autonomy"] = v
	}
	if v, ok := flags["budget"]; ok {
		body["budget_usd"] = v
	}

	c := newClient()
	respBody, status, err := c.do(http.MethodPost, "/v1/runs", body)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("create run: status %d: %s", status, respBody)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return err
	}
	fmt.Printf("run_id: %s\n", out.RunID)
	fmt.Printf("watch: %s/v1/runs/%s\n", c.baseURL, out.RunID)
	return nil
}

func cmdApprovals(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("approvals requires a subcommand: show|list|grant|deny")
	}
	c := newClient()
	switch args[0] {
	case "list":
		return approvalsList(c)
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("approvals show requires an approval id")
		}
		return approvalsShow(c, args[1])
	case "grant":
		if len(args) < 2 {
			return fmt.Errorf("approvals grant requires an approval id")
		}
		_, flags := parseFlags(args[2:])
		return approvalsGrant(c, args[1], flags["modify"])
	case "deny":
		if len(args) < 2 {
			return fmt.Errorf("approvals deny requires an approval id")
		}
		_, flags := parseFlags(args[2:])
		return approvalsDeny(c, args[1], flags["reason"])
	default:
		return fmt.Errorf("unknown approvals subcommand %q", args[0])
	}
}

func approvalsList(c *client) error {
	body, status, err := c.do(http.MethodGet, "/v1/approvals", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("list approvals: status %d: %s", status, body)
	}
	return printPretty(body)
}

// approvalsShow renders the ContextPackage a human approver needs — the
// tool, its effect class, and its ORIGINAL input fields — never a bare
// approval UUID (README §5's Phase 5 demo text, verbatim).
func approvalsShow(c *client, id string) error {
	body, status, err := c.do(http.MethodGet, "/v1/approvals/"+id, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("get approval: status %d: %s", status, body)
	}
	return printPretty(body)
}

func approvalsGrant(c *client, id, modifiedInput string) error {
	var body any
	if modifiedInput != "" {
		body = map[string]json.RawMessage{"modified_input": json.RawMessage(modifiedInput)}
	}
	respBody, status, err := c.do(http.MethodPost, "/v1/approvals/"+id+"/grant", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("grant approval: status %d: %s", status, respBody)
	}
	return printPretty(respBody)
}

func approvalsDeny(c *client, id, reason string) error {
	body := map[string]string{"reason": reason}
	respBody, status, err := c.do(http.MethodPost, "/v1/approvals/"+id+"/deny", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("deny approval: status %d: %s", status, respBody)
	}
	return printPretty(respBody)
}

// cmdCancel drives POST /v1/runs/{id}/cancel (README task 6.9) — the sole
// producer of the "aborted" terminal reason.
func cmdCancel(args []string) error {
	positional, flags := parseFlags(args)
	if len(positional) != 1 {
		return fmt.Errorf(`cancel requires exactly one argument: nexusctl cancel <session-id>`)
	}
	c := newClient()
	body := map[string]string{}
	if v, ok := flags["reason"]; ok {
		body["reason"] = v
	}
	respBody, status, err := c.do(http.MethodPost, "/v1/runs/"+positional[0]+"/cancel", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("cancel: status %d: %s", status, respBody)
	}
	return printPretty(respBody)
}

// cmdSteer drives POST /v1/runs/{id}/steer (README task 6.9).
func cmdSteer(args []string) error {
	positional, _ := parseFlags(args)
	if len(positional) != 2 {
		return fmt.Errorf(`steer requires exactly two arguments: nexusctl steer <session-id> "<input>"`)
	}
	c := newClient()
	respBody, status, err := c.do(http.MethodPost, "/v1/runs/"+positional[0]+"/steer", map[string]string{"input": positional[1]})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("steer: status %d: %s", status, respBody)
	}
	return printPretty(respBody)
}

// cmdFork drives POST /v1/runs/{id}/fork (README task 6.11) — README §6's
// own demo line: `nexusctl fork <session> --at 42 --model haiku`.
func cmdFork(args []string) error {
	positional, flags := parseFlags(args)
	if len(positional) != 1 {
		return fmt.Errorf(`fork requires exactly one argument: nexusctl fork <session-id> --at=<seq> [--model=<model-id>]`)
	}
	atSeq, ok := flags["at"]
	if !ok {
		return fmt.Errorf("fork requires --at=<seq>")
	}
	// at_seq must be a JSON number, not a string — parseFlags only ever
	// gives us strings, so parse and re-encode it rather than growing
	// parseFlags a type system for one call site.
	seq, err := strconv.ParseInt(atSeq, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid --at=%q: %w", atSeq, err)
	}
	body := map[string]any{"at_seq": seq}
	if v, ok := flags["model"]; ok {
		body["model"] = v
	}

	c := newClient()
	respBody, status, err := c.do(http.MethodPost, "/v1/runs/"+positional[0]+"/fork", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("fork: status %d: %s", status, respBody)
	}
	return printPretty(respBody)
}

func printPretty(body []byte) error {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Println(string(body)) // not JSON — print raw rather than fail the whole command
		return nil
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(pretty))
	return nil
}
