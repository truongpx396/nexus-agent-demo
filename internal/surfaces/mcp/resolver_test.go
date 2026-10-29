package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/mcp"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// fakeDirectory is a ServerDirectory over a map, so these tests pin the
// resolver's own policy — what resolves, what is refused, as whom a server is
// called — separately from what the Postgres adapter reads.
type fakeDirectory struct {
	servers  map[string]mcp.Server
	tokens   map[string]string
	user     uuid.UUID
	findErr  error
	tokenErr map[string]error

	sessionLookups int
}

func (d *fakeDirectory) Admitted(context.Context, uuid.UUID) ([]mcp.Server, error) {
	var out []mcp.Server
	for _, s := range d.servers {
		if s.Status == "admitted" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (d *fakeDirectory) Find(_ context.Context, _ uuid.UUID, name string) (mcp.Server, bool, error) {
	s, ok := d.servers[name]
	return s, ok, d.findErr
}

func (d *fakeDirectory) StaticToken(_ context.Context, _ uuid.UUID, name string) (string, error) {
	return d.tokens[name], d.tokenErr[name]
}

func (d *fakeDirectory) SessionUser(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
	d.sessionLookups++
	return d.user, nil
}

type tokenSource struct{ gotUser uuid.UUID }

func (t *tokenSource) AccessToken(_ context.Context, _, userID uuid.UUID, _ string) (string, error) {
	t.gotUser = userID
	return "oauth-for-" + userID.String(), nil
}

// remote is a minimal MCP server that counts tools/list calls and remembers
// the Authorization header it last saw.
type remote struct {
	*httptest.Server
	lists atomic.Int32
	auth  atomic.Value // string
}

func newRemote(t *testing.T, tool mcp.Tool) *remote {
	t.Helper()
	r := &remote{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.auth.Store(req.Header.Get("authorization"))
		var body struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		w.Header().Set("content-type", "application/json")
		switch body.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": map[string]any{}})
		case "tools/list":
			r.lists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": map[string]any{"tools": []mcp.Tool{tool}}})
		default:
			http.Error(w, "unknown method", http.StatusNotImplemented)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

var issueTool = mcp.Tool{Name: "create_issue", Description: "opens an issue", InputSchema: json.RawMessage(`{"type":"object"}`)}

func refFor(server string, tool mcp.Tool) tools.ToolRef {
	return tools.ToolRef{Namespace: "mcp/" + server, Name: tool.Name, Version: "v" + tool.SchemaDigest()}
}

func resolver(d *fakeDirectory, tokens mcp.TokenSource) *mcp.Resolver {
	return &mcp.Resolver{Servers: d, Tokens: tokens, AllowedHosts: []string{"*"}}
}

func TestResolve_RefsThatAreNotAnAdmittedServersToolDoNotResolve(t *testing.T) {
	srv := newRemote(t, issueTool)
	d := &fakeDirectory{servers: map[string]mcp.Server{
		"gh":  {ServerID: uuid.New(), Name: "gh", BaseURL: srv.URL, AuthKind: "none", Status: "admitted"},
		"off": {ServerID: uuid.New(), Name: "off", BaseURL: srv.URL, AuthKind: "none", Status: "disabled"},
	}}
	r := resolver(d, nil)

	cases := map[string]tools.ToolRef{
		"not an MCP namespace":      {Namespace: "platform", Name: "create_issue", Version: "v1"},
		"an unknown server":         refFor("nope", issueTool),
		"a server that is disabled": refFor("off", issueTool),
		"a tool the server lacks":   {Namespace: "mcp/gh", Name: "delete_repo", Version: "v1"},
		"a schema that has drifted": {Namespace: "mcp/gh", Name: "create_issue", Version: "v00000000"},
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			tool, ok, err := r.Resolve(context.Background(), uuid.New(), uuid.New(), ref)
			if err != nil || ok || tool != nil {
				t.Errorf("Resolve = (%v, %v, %v), want (nil, false, nil): every refusal must look like the tool not existing", tool, ok, err)
			}
		})
	}
}

func TestResolve_AnAdmittedServersToolResolvesWhenItsSchemaStillMatches(t *testing.T) {
	srv := newRemote(t, issueTool)
	d := &fakeDirectory{servers: map[string]mcp.Server{"gh": {ServerID: uuid.New(), Name: "gh", BaseURL: srv.URL, AuthKind: "none", Status: "admitted"}}}

	tool, ok, err := resolver(d, nil).Resolve(context.Background(), uuid.New(), uuid.New(), refFor("gh", issueTool))
	if err != nil || !ok || tool == nil {
		t.Fatalf("Resolve = (%v, %v, %v), want a resolved tool", tool, ok, err)
	}
	if tool.ID() != refFor("gh", issueTool) {
		t.Errorf("tool ID = %+v, want the ref it was resolved from", tool.ID())
	}
	if d.sessionLookups != 0 {
		t.Error("the calling session was looked up for a server that does not authenticate as a user")
	}
}

func TestResolve_AStaticServerIsCalledWithItsOpenedToken(t *testing.T) {
	srv := newRemote(t, issueTool)
	d := &fakeDirectory{
		servers: map[string]mcp.Server{"gh": {ServerID: uuid.New(), Name: "gh", BaseURL: srv.URL, AuthKind: "bearer_static", Status: "admitted"}},
		tokens:  map[string]string{"gh": "s3cret"},
	}
	if _, ok, err := resolver(d, nil).Resolve(context.Background(), uuid.New(), uuid.New(), refFor("gh", issueTool)); err != nil || !ok {
		t.Fatalf("Resolve = (%v, %v)", ok, err)
	}
	if got := srv.auth.Load(); got != "Bearer s3cret" {
		t.Errorf("remote saw Authorization %q, want the directory's opened token as a bearer", got)
	}
}

func TestResolve_AnOAuthServerAuthenticatesAsTheCallingSessionsUser(t *testing.T) {
	srv := newRemote(t, issueTool)
	user := uuid.New()
	d := &fakeDirectory{
		servers: map[string]mcp.Server{"gh": {ServerID: uuid.New(), Name: "gh", BaseURL: srv.URL, AuthKind: "oauth_connector", OAuthProvider: "github", Status: "admitted"}},
		user:    user,
	}
	ts := &tokenSource{}
	if _, ok, err := resolver(d, ts).Resolve(context.Background(), uuid.New(), uuid.New(), refFor("gh", issueTool)); err != nil || !ok {
		t.Fatalf("Resolve = (%v, %v)", ok, err)
	}
	if ts.gotUser != user {
		t.Errorf("token requested for user %s, want the session's user %s: never a service account", ts.gotUser, user)
	}
	if got := srv.auth.Load(); got != "Bearer oauth-for-"+user.String() {
		t.Errorf("remote saw Authorization %q, want the user's oauth token", got)
	}
}

func TestResolve_ADirectoryFailureIsAnErrorNotARefusal(t *testing.T) {
	d := &fakeDirectory{findErr: errors.New("db down")}
	_, ok, err := resolver(d, nil).Resolve(context.Background(), uuid.New(), uuid.New(), refFor("gh", issueTool))
	if err == nil || ok {
		t.Errorf("Resolve = (ok=%v, err=%v), want an error: an outage must not read as \"no such tool\"", ok, err)
	}
}

func TestResolve_AServersToolListIsCachedBetweenCalls(t *testing.T) {
	srv := newRemote(t, issueTool)
	d := &fakeDirectory{servers: map[string]mcp.Server{"gh": {ServerID: uuid.New(), Name: "gh", BaseURL: srv.URL, AuthKind: "none", Status: "admitted"}}}
	r := resolver(d, nil)
	for i := 0; i < 3; i++ {
		if _, ok, err := r.Resolve(context.Background(), uuid.New(), uuid.New(), refFor("gh", issueTool)); err != nil || !ok {
			t.Fatalf("Resolve #%d = (%v, %v)", i+1, ok, err)
		}
	}
	if got := srv.lists.Load(); got != 1 {
		t.Errorf("tools/list fetched %d times for 3 resolutions, want 1: every tool call must not cost a round trip", got)
	}
}

func TestPort_OneBrokenServerDoesNotTakeDownTheCatalog(t *testing.T) {
	good, broken := newRemote(t, issueTool), newRemote(t, mcp.Tool{Name: "other", InputSchema: json.RawMessage(`{}`)})
	d := &fakeDirectory{
		servers: map[string]mcp.Server{
			"good":   {ServerID: uuid.New(), Name: "good", BaseURL: good.URL, AuthKind: "none", Status: "admitted"},
			"broken": {ServerID: uuid.New(), Name: "broken", BaseURL: broken.URL, AuthKind: "bearer_static", Status: "admitted"},
		},
		tokenErr: map[string]error{"broken": errors.New("key shredded")},
	}
	schemas, digest, err := (&mcp.Port{Resolver: resolver(d, nil)}).Resolve(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(schemas) != 1 || schemas[0].Name != "mcp/good/create_issue@v"+issueTool.SchemaDigest() {
		t.Errorf("catalog = %+v, want only the working server's tool", schemas)
	}
	if len(digest) == 0 {
		t.Error("no catalog digest")
	}
}
