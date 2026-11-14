package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/truongpx396/nexus-agent/internal/hooks/exechook"
	"github.com/truongpx396/nexus-agent/internal/hooks/httphook"
	runctlpg "github.com/truongpx396/nexus-agent/internal/runctl/postgres"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"

	auditpg "github.com/truongpx396/nexus-agent/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent/internal/config"
	"github.com/truongpx396/nexus-agent/internal/connectors"
	connoauth "github.com/truongpx396/nexus-agent/internal/connectors/oauth2"
	"github.com/truongpx396/nexus-agent/internal/cost"
	cryptopg "github.com/truongpx396/nexus-agent/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent/internal/delegate"
	delegatetool "github.com/truongpx396/nexus-agent/internal/delegate/tool"
	"github.com/truongpx396/nexus-agent/internal/hooks"
	"github.com/truongpx396/nexus-agent/internal/permissions"
	"github.com/truongpx396/nexus-agent/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent/internal/provider"
	"github.com/truongpx396/nexus-agent/internal/retrieval"
	retrievalpg "github.com/truongpx396/nexus-agent/internal/retrieval/postgres"
	retrievaltool "github.com/truongpx396/nexus-agent/internal/retrieval/tool"
	"github.com/truongpx396/nexus-agent/internal/runctl"
	runspg "github.com/truongpx396/nexus-agent/internal/runs/postgres"
	"github.com/truongpx396/nexus-agent/internal/sandbox"
	"github.com/truongpx396/nexus-agent/internal/skills"
	"github.com/truongpx396/nexus-agent/internal/store"
	"github.com/truongpx396/nexus-agent/internal/surfaces/mcp"
	mcppg "github.com/truongpx396/nexus-agent/internal/surfaces/mcp/postgres"
	"github.com/truongpx396/nexus-agent/internal/teams"
	teamstool "github.com/truongpx396/nexus-agent/internal/teams/tool"
	"github.com/truongpx396/nexus-agent/internal/tools"
	"github.com/truongpx396/nexus-agent/internal/tools/builtin"
	toolspg "github.com/truongpx396/nexus-agent/internal/tools/postgres"
)

// toolStack is what newToolPipeline assembles: the pipeline every tool call goes
// through, the resident catalog the model is shown, the digest that pins it, and
// the admitted skill bundles and MCP port the run API resolves a session's tools
// against.
type toolStack struct {
	pipeline       *tools.Pipeline
	catalog        []provider.ToolSchema
	manifestDigest []byte
	skillBundles   []skills.SkillBundle
	mcp            *mcp.Port
}

// newToolPipeline wires the Phase 3 tool pipeline: the resident catalog
// (the builtin tools, now including Phase 7's activate_skill/
// read_skill_file), the permission chain's tenant-independent config, and
// the hook dispatcher. st/keyStore/chain wire Phase 5's derived-artifact
// tracking (task 5.4) into BudgetResult's spill path, and (Phase 7) back
// runctl.NewSkillEventRecorder for skill_activated/skill_capability_ignored.
// Returns the admitted skill bundles too, so main() can wire
// nexusdSkillSetPort without reloading them.
// newToolPipeline builds the resident catalog. delegations is constructed
// by the caller (serve()) BEFORE this runs and Wire()d AFTER — the same
// two-phase dependency this file already has for everything downstream of a
// *kernel.Kernel that the kernel's OWN construction also needs a pipeline
// for (Claims/skill events face an easier version of this: they never
// needed the Kernel itself, only Store/Keys/Chain). platform/delegate's own
// Ledger/Spawner fields are satisfied by delegations/nexusdDelegationSpawner
// immediately — Wire only needs to land before the FIRST real dispatch,
// which is well after serve() finishes wiring everything.
func newToolPipeline(st *store.Store, cfgStore config.Loader, keyStore *cryptopg.KeyStore, chain *auditpg.Chain, delegations *delegate.Delegations, teamsSvc *teams.Service, vault *connectors.Vault, gate *cost.Gate) (toolStack, error) {
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "nexusd"); err != nil {
		return toolStack{}, fmt.Errorf("declare platform namespace: %w", err)
	}

	skillCatalog, admittedBundles, scriptTools, err := loadSkillCatalog(reg)
	if err != nil {
		return toolStack{}, err
	}

	// NEXUS_WEB_FETCH_ALLOWLIST is the ONE egress allowlist README task
	// 5.13 established, extended by task 11.9 to cover platform/
	// connector_fetch's own outbound calls and every admitted MCP server's
	// base_url — reusing hostAllowed's fail-closed semantics rather than a
	// second allowlist mechanism per host class.
	webFetchAllowlist := strings.Split(envOr("NEXUS_WEB_FETCH_ALLOWLIST", ""), ",")

	mcpResolver := &mcp.Resolver{Servers: mcppg.New(st, keyStore), Tokens: vault, AllowedHosts: webFetchAllowlist}
	mcpPort := &mcp.Port{Resolver: mcpResolver}

	// Phase 12: platform/retrieve's own Searcher, backed by a Retriever
	// wired to the same Store/Gate everything else here shares and to
	// newEmbedder()'s embedding port (task 12.4/12.6). ModelID is a price-
	// book/cost-record label, not a live credential (internal/retrieval.
	// Retriever.ModelID's own doc comment).
	retriever := &retrieval.Retriever{
		Index: retrievalpg.NewIndex(st), Gate: gate, Embedder: newEmbedder(), ModelID: "fake-embedder-v1", TopK: 5,
	}

	builtinTools := []tools.Tool{
		builtin.FileRead{},
		builtin.FileWrite{},
		builtin.FileSearch{},
		builtin.Shell{},
		builtin.WebFetch{AllowedHosts: webFetchAllowlist},
		builtin.ActivateSkill{
			Catalog:  skillCatalog,
			Registry: reg,
			Events:   runctl.NewSkillEventRecorder(runctlpg.New(st, keyStore, chain)),
			Admitted: func(tenantID uuid.UUID) []string {
				cfg, err := cfgStore.Load(context.Background(), tenantID)
				if err != nil {
					log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: load tenant config for skill admission check")
					return nil // fail closed: an unreadable config admits nothing
				}
				return cfg.AdmittedSkillIDs
			},
		},
		builtin.ReadSkillFile{Catalog: skillCatalog},
		delegatetool.Delegate{Ledger: delegations, Spawner: delegations, Registry: reg},
	}
	builtinTools = append(builtinTools, teamstool.Tools(teamsSvc)...)
	builtinTools = append(builtinTools,
		builtin.ConnectorFetch{Tokens: vault, Sessions: nexusdSessionLookup{reader: runspg.Reader{Store: st, KeyStore: keyStore}}, AllowedHosts: webFetchAllowlist},
		retrievaltool.NewRetrieve(retriever),
		builtin.AskClarification{},
	)
	// platform/web_crawl (README specs/README.md Phase 16, task 16.1) is
	// registered only when NEXUS_CRAWL4AI_URL is actually configured — the
	// same "absent means off" convention newConnectorRegistry already uses
	// for an unconfigured OAuth provider, rather than shipping a tool that is
	// always present but always fails closed with "not configured."
	if crawl4aiURL := envOr("NEXUS_CRAWL4AI_URL", ""); crawl4aiURL != "" {
		builtinTools = append(builtinTools, builtin.WebCrawl{
			BaseURL:      crawl4aiURL,
			APIToken:     envOr("NEXUS_CRAWL4AI_API_TOKEN", ""),
			AllowedHosts: webFetchAllowlist,
		})
	}
	toolRefs := make([]string, 0, len(builtinTools)+len(scriptTools))
	catalog := make([]provider.ToolSchema, 0, len(builtinTools)+len(scriptTools))
	for _, t := range builtinTools {
		if err := reg.Register(t); err != nil {
			return toolStack{}, fmt.Errorf("register tool %s: %w", t.ID(), err)
		}
		status, findings := tools.Scan(t.Descriptor())
		if err := reg.SetAdmissionStatus(t.ID(), status); err != nil {
			return toolStack{}, fmt.Errorf("set admission status for %s: %w", t.ID(), err)
		}
		if status != tools.AdmissionClean {
			return toolStack{}, fmt.Errorf("builtin tool %s failed admission (%s): %v", t.ID(), status, findings)
		}
		toolRefs = append(toolRefs, t.ID().String())
		d := t.Descriptor()
		catalog = append(catalog, provider.ToolSchema{Name: d.ID.String(), Description: d.Description, InputSchema: d.InputSchema})
	}
	// Skill scripts were already registered+admitted inside
	// loadSkillCatalog (it needs reg to declare the "skill" namespace
	// before this function can build anything from it) — this loop only
	// adds them to the resident catalog/tool-profile, never re-registers.
	for _, t := range scriptTools {
		toolRefs = append(toolRefs, t.ID().String())
		d := t.Descriptor()
		catalog = append(catalog, provider.ToolSchema{Name: d.ID.String(), Description: d.Description, InputSchema: d.InputSchema})
	}

	manifest := tools.BuildManifest(reg)
	permChain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, toolRefs...)}},
		Safety:   safety.NewClassifier(safety.DefaultRules(), demoSafetyModel{}, 0),
	})

	workspaceRoot := envOr("NEXUS_WORKSPACE_ROOT", ".dev/workspaces")
	sandboxFactory, err := newSandboxFactory(workspaceRoot, devMode)
	if err != nil {
		return toolStack{}, err
	}
	pipeline := tools.NewPipeline(tools.PipelineConfig{
		Registry:         reg,
		Manifest:         manifest,
		Chain:            permChain,
		Hooks:            newHookDispatcher(),
		Blobs:            tools.BlobStore{Dir: envOr("NEXUS_BLOB_DIR", ".dev/blobs")},
		WorkspaceRoot:    workspaceRoot,
		DerivedArtifacts: toolspg.NewDerivedArtifactRecorder(st),
		SandboxFactory:   sandboxFactory,
		// Claims (README task 6.6): write-ahead idempotency for every
		// non-read-only tool call. Built here (rather than passed in) so
		// newToolPipeline stays the one place that wires the resident
		// catalog — runctl.NewClaimTracker takes st/keyStore/chain
		// directly, not a *runctl.Control, precisely so this can happen
		// before a Control (which needs the *kernel.Kernel this very
		// pipeline feeds into) exists.
		Claims: runctl.NewClaimTracker(runctlpg.New(st, keyStore, chain)),
		// Dynamic (README task 11.1): a ref the static Manifest above
		// doesn't know about — every per-tenant MCP tool, by construction
		// — falls through to mcpResolver instead of failing immediately.
		Dynamic: mcpResolver,
	})
	return toolStack{pipeline: pipeline, catalog: catalog, manifestDigest: manifest.Digest, skillBundles: admittedBundles, mcp: mcpPort}, nil
}

// newConnectorRegistry builds internal/connectors.Registry from
// NEXUS_OAUTH_PROVIDERS (comma-separated provider names) and, per name,
// NEXUS_OAUTH_<NAME>_{CLIENT_ID,CLIENT_SECRET,AUTH_URL,TOKEN_URL,
// REDIRECT_URL,SCOPES} — fully generic rather than hardcoding any specific
// provider's endpoint, the same "config, not forks" discipline
// tenant_configs' own admitted-set columns already follow. Which
// PROVIDERS EXIST is this process-wide config; which providers a given
// TENANT may actually use is config.TenantConfig.AdmittedConnectorProviders
// (internal/connectors.Vault.BeginAuth's own second check). An unset
// NEXUS_OAUTH_PROVIDERS registers nothing — the honest empty default this
// codebase's other optional integrations (skills signing, sandboxing) also
// use.
func newConnectorRegistry() *connoauth.Registry {
	names := strings.Split(envOr("NEXUS_OAUTH_PROVIDERS", ""), ",")
	var providers []connoauth.Provider
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		upper := strings.ToUpper(name)
		clientID := envOr("NEXUS_OAUTH_"+upper+"_CLIENT_ID", "")
		if clientID == "" {
			continue // unconfigured — skip rather than register a provider that can never exchange a code
		}
		var scopes []string
		if raw := envOr("NEXUS_OAUTH_"+upper+"_SCOPES", ""); raw != "" {
			scopes = strings.Split(raw, ",")
		}
		providers = append(providers, connoauth.Provider{
			Name:         name,
			ClientID:     clientID,
			ClientSecret: envOr("NEXUS_OAUTH_"+upper+"_CLIENT_SECRET", ""),
			RedirectURL:  envOr("NEXUS_OAUTH_"+upper+"_REDIRECT_URL", ""),
			Scopes:       scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:  envOr("NEXUS_OAUTH_"+upper+"_AUTH_URL", ""),
				TokenURL: envOr("NEXUS_OAUTH_"+upper+"_TOKEN_URL", ""),
			},
		})
	}
	return connoauth.NewRegistry(providers...)
}

// devSkillsSigningPubkey is the public half of the fixed, deterministic
// ed25519 dev key specs/README.md Phase 16 (task 16.7) signed
// skills/web-research and skills/sandboxed-code with — the same "fixed,
// low-entropy dev-only value" spirit as deploy/docker-compose.yml's
// postgres://nexus:nexus@... and deploy/docker-compose.local-llm.yml's
// fixed Langfuse keypair. A public key carries no confidentiality
// requirement, so defaulting NEXUS_SKILLS_SIGNING_PUBKEY to it (rather than
// leaving it empty) is what makes the two in-repo demo skills trusted out of
// the box, the same zero-setup posture NEXUS_PROVIDER=fake and
// loadOrGenerateKEK's --dev branch already hold this binary to — an
// operator who signs their own bundles overrides this env var, never edits
// the default. docs/agentic-capabilities.md documents the regeneration
// recipe (sign with a real, private key; publish only the public half here).
const devSkillsSigningPubkey = "O25T/FllLc+s5DjZvNhcrLzo13358AdkbfLpYX0jEFc="

// loadSkillCatalog reads NEXUS_SKILLS_ROOT (default "skills" — a tracked
// repo directory, not .dev/: skill bundles are durable, reviewed content
// like evals/corpus/*.yaml, not machine-local generated secrets), admits
// every bundle that scans clean AND carries a valid signature under
// NEXUS_SKILLS_SIGNING_PUBKEY (base64 ed25519 public key, defaulting to
// devSkillsSigningPubkey above) — a bundle failing either check is SKIPPED
// and logged, not fataled: skill bundles are less-trusted content than a
// first-party builtin tool, unlike the builtin loop above which fatals on a
// bad admission (README task 7.5's "the whole bundle is refused" scoped to
// that one bundle, not the process). A bundle with a script gets that
// script registered as a real tool under the "skill" namespace; if
// registration fails (e.g. a namespace/ref collision), the WHOLE bundle is
// dropped, per task 7.5 — never a bundle with a body but a silently
// missing tool.
func loadSkillCatalog(reg *tools.Registry) (*skills.Catalog, []skills.SkillBundle, []tools.Tool, error) {
	bundles, err := skills.LoadBundles(envOr("NEXUS_SKILLS_ROOT", "skills"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load skill bundles: %w", err)
	}
	if len(bundles) == 0 {
		return skills.NewCatalog(nil), nil, nil, nil
	}

	var pubKey ed25519.PublicKey
	if raw := envOr("NEXUS_SKILLS_SIGNING_PUBKEY", devSkillsSigningPubkey); raw != "" {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("decode NEXUS_SKILLS_SIGNING_PUBKEY: %w", err)
		}
		pubKey = ed25519.PublicKey(decoded)
	}

	if err := reg.DeclareNamespace("skill", "nexusd"); err != nil {
		return nil, nil, nil, fmt.Errorf("declare skill namespace: %w", err)
	}

	var admitted []skills.SkillBundle
	var scriptTools []tools.Tool
	for _, b := range bundles {
		if pubKey == nil || !skills.VerifySignature(b, pubKey) {
			log.Warn().Any("skill_id", b.SkillID).Msg("nexusd: skipped a skill bundle with a missing or invalid signature")
			continue
		}
		status, findings := skills.ScanBundle(b)
		if status != tools.AdmissionClean {
			log.Warn().Any("skill_id", b.SkillID).Any("status", status).Any("findings", findings).Msg("nexusd: skipped a skill bundle that failed admission scanning")
			continue
		}
		if b.HasScript() {
			script := skills.ScriptTool{SkillID: b.SkillID, Description: b.Description, Content: b.ScriptContent}
			if err := reg.Register(script); err != nil {
				log.Warn().Err(err).Any("skill_id", b.SkillID).Msg("nexusd: skipped a skill bundle whose script failed to register as a tool")
				continue
			}
			if err := reg.SetAdmissionStatus(script.ID(), tools.AdmissionClean); err != nil {
				log.Warn().Err(err).Any("skill_id", b.SkillID).Msg("nexusd: skipped a skill bundle whose script tool could not be admitted")
				continue
			}
			scriptTools = append(scriptTools, script)
		}
		admitted = append(admitted, b)
	}
	return skills.NewCatalog(admitted), admitted, scriptTools, nil
}

// newSandboxFactory wires platform/shell to run inside Docker (README task
// 5.12) when NEXUS_SANDBOX=docker, or inside an OpenSandbox sandbox
// (specs/README.md Phase 16, task 16.4) when NEXUS_SANDBOX=opensandbox and
// NEXUS_OPENSANDBOX_URL is configured.
//
// Outside --dev it fails closed, like every other security-critical setting
// (serve's loadServerConfig): the variable must name one of the two, and a Docker
// daemon that cannot be reached, or an OpenSandbox with no URL, is an error. With no
// sandbox platform/shell runs /bin/sh on the host as the nexusd user, which is not
// something a deployment should arrive at by leaving a variable unset or a daemon
// down. In --dev it keeps the zero-setup path (`make up && make run` on a machine
// with no Docker daemon and no OpenSandbox server): unset, or a misconfiguration,
// logs a warning and leaves the shell unsandboxed, with the clean environment
// builtin.Shell gives it.
func newSandboxFactory(workspaceRoot string, dev bool) (func(uuid.UUID) tools.SandboxExec, error) {
	kind := envOr("NEXUS_SANDBOX", "")
	if !dev && kind != "docker" && kind != "opensandbox" {
		return nil, fmt.Errorf("NEXUS_SANDBOX must be %q or %q outside --dev (it is %q): without a sandbox platform/shell would run commands on the host as the nexusd user", "docker", "opensandbox", kind)
	}
	switch kind {
	case "docker":
		docker, err := sandbox.NewDocker()
		if err != nil {
			if !dev {
				return nil, fmt.Errorf("NEXUS_SANDBOX=docker but connecting to Docker failed: %w", err)
			}
			log.Warn().Err(err).Msg("nexusd: NEXUS_SANDBOX=docker but connecting to Docker failed; platform/shell stays unsandboxed")
			return nil, nil
		}
		return func(sessionID uuid.UUID) tools.SandboxExec {
			return sandbox.SessionSandbox{
				Docker: docker,
				Config: sandbox.Config{WorkspaceDir: filepath.Join(workspaceRoot, sessionID.String())},
			}
		}, nil
	case "opensandbox":
		domain := envOr("NEXUS_OPENSANDBOX_URL", "")
		if domain == "" {
			if !dev {
				return nil, errors.New("NEXUS_SANDBOX=opensandbox but NEXUS_OPENSANDBOX_URL is unset")
			}
			log.Warn().Msg("nexusd: NEXUS_SANDBOX=opensandbox but NEXUS_OPENSANDBOX_URL is unset; platform/shell stays unsandboxed")
			return nil, nil
		}
		protocol, host, ok := strings.Cut(domain, "://")
		if !ok {
			protocol, host = "http", domain
		}
		apiKey := envOr("NEXUS_OPENSANDBOX_API_KEY", "")
		return func(uuid.UUID) tools.SandboxExec {
			// Unlike Docker (one bind-mounted host directory per session),
			// OpenSandbox's sandbox itself IS the per-call scope — no
			// session-keyed WorkspaceDir to thread through, one fresh
			// sandbox per Exec call (opensandbox.go's own doc comment).
			return sandbox.OpenSandboxSession{
				Config: sandbox.OpenSandboxConfig{Domain: host, Protocol: protocol, APIKey: apiKey},
			}
		}, nil
	default:
		return nil, nil // --dev with NEXUS_SANDBOX unset
	}
}

// demoSafetyModel stands in for Gate 3's model leg (internal/permissions/
// safety.ModelClassifier): Phase 3 ships no real model-backed classifier —
// wiring one through the ordinary Provider port is future work, not a
// Phase 3 task — so this always defers instead of asking about every call
// safety.DefaultRules doesn't recognize, which would otherwise swamp the
// "governed agent" demo in approval requests for plainly harmless calls.
type demoSafetyModel struct{}

func (demoSafetyModel) Classify(context.Context, string, string) (safety.Verdict, string, error) {
	return safety.VerdictDefer, "no real safety model configured (demo default)", nil
}

// newHookDispatcher is the hook layer with every handler kind registered:
// internal/hooks owns the port, the chain and the I/O-free prompt kind, and the
// command and http kinds are adapters only the composition root links in.
func newHookDispatcher() *hooks.Dispatcher {
	d := hooks.NewDispatcher()
	d.Handlers[hooks.KindCommand] = exechook.Handler{}
	d.Handlers[hooks.KindHTTP] = httphook.Handler{}
	return d
}
