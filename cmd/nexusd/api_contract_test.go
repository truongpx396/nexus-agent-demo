package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	runsrest "github.com/truongpx396/nexus-agent-demo/internal/runs/rest"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

// pathParam matches an OpenAPI path parameter, "{id}".
var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// opsTag marks the operations nexusd serves itself, outside the REST kit (the
// probes and /metrics): documented in api/openapi.yaml, with no code generated.
const opsTag = "ops"

func loadAPISpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("load api/openapi.yaml: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("api/openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

// The spec is the contract and the generated interfaces are what mount the
// routes, so the two agree by construction for an operation that was generated.
// This closes the other directions: an operation in the spec whose tag has no
// generator config or whose module is not composed, and a route someone mounted
// by hand without describing it.
func TestAPISpec_DescribesExactlyTheRoutesTheRESTKitMounts(t *testing.T) {
	doc := loadAPISpec(t)

	var described []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !slices.Contains(op.Tags, opsTag) {
				described = append(described, method+" "+path)
			}
		}
	}

	var mounted []string
	for _, r := range restkit.NewServer(nil, restModules(runsrest.NewHandler(nil, nil), &services{}, nil)...).Routes() {
		mounted = append(mounted, r.Method+" "+r.Path)
	}

	slices.Sort(described)
	slices.Sort(mounted)
	for _, route := range described {
		if !slices.Contains(mounted, route) {
			t.Errorf("api/openapi.yaml describes %s but no composed module mounts it: tag it for a feature with a generator config (oapi-codegen.yaml) and run `make generate`", route)
		}
	}
	for _, route := range mounted {
		if !slices.Contains(described, route) {
			t.Errorf("%s is mounted but api/openapi.yaml does not describe it: add the operation, run `make generate` and implement it", route)
		}
	}
	if len(described) == 0 {
		t.Fatal("the spec describes no REST operation: the test would pass on nothing")
	}
}

// Every route the kit mounts needs a bearer token, and the probes need none, so
// the spec's security declarations are checkable against what the kit does.
func TestAPISpec_DeclaresBearerAuthOnEveryKitRouteAndNoneOnTheProbes(t *testing.T) {
	doc := loadAPISpec(t)
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			security := doc.Security
			if op.Security != nil {
				security = *op.Security
			}
			authenticated := len(security) > 0
			if wantAuth := !slices.Contains(op.Tags, opsTag); authenticated != wantAuth {
				t.Errorf("%s %s: security declares authentication = %v, want %v", method, path, authenticated, wantAuth)
			}
		}
	}
}

// A generated route must be as closed as a hand-mounted one: asking without a
// token is a 401 and not a 404 (the route is there) or a 200 (it is open).
func TestAPISpec_EveryDescribedRouteAnswers401WithoutAToken(t *testing.T) {
	doc := loadAPISpec(t)
	h := restkit.NewServer(resttest.Verifier{}, restModules(runsrest.NewHandler(nil, nil), &services{}, nil)...).Handler()

	checked := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if slices.Contains(op.Tags, opsTag) {
				continue
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, pathParam.ReplaceAllString(path, uuid.NewString()), nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s without a token = %d, want 401", method, path, rec.Code)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no operation was checked")
	}
}
