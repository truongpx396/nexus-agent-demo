package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/retrieval"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/sandbox"
	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/mcp"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
)

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
func newToolPipeline(st *store.Store, keyStore *crypto.KeyStore, chain *audit.Chain, delegations *delegate.Delegations, teamsSvc *teams.Service, vault *connectors.Vault, gate *cost.Gate) (*tools.Pipeline, []provider.ToolSchema, []byte, []skills.SkillBundle, *mcp.Port) {
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "nexusd"); err != nil {
		fatalf("declare platform namespace: %v", err)
	}

	skillCatalog, admittedBundles, scriptTools := loadSkillCatalog(reg)

	// NEXUS_WEB_FETCH_ALLOWLIST is the ONE egress allowlist README task
	// 5.13 established, extended by task 11.9 to cover platform/
	// connector_fetch's own outbound calls and every admitted MCP server's
	// base_url — reusing hostAllowed's fail-closed semantics rather than a
	// second allowlist mechanism per host class.
	webFetchAllowlist := strings.Split(envOr("NEXUS_WEB_FETCH_ALLOWLIST", ""), ",")

	mcpResolver := &mcp.Resolver{Store: st, Keys: keyStore, Tokens: vault, AllowedHosts: webFetchAllowlist}
	mcpPort := &mcp.Port{Resolver: mcpResolver}

	// Phase 12: platform/retrieve's own Searcher, backed by a Retriever
	// wired to the same Store/Gate everything else here shares and to
	// newEmbedder()'s embedding port (task 12.4/12.6). ModelID is a price-
	// book/cost-record label, not a live credential (internal/retrieval.
	// Retriever.ModelID's own doc comment).
	retriever := &retrieval.Retriever{
		Store: st, Gate: gate, Embedder: newEmbedder(), ModelID: "fake-embedder-v1", TopK: 5,
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
			Events:   runctl.NewSkillEventRecorder(st, keyStore, chain),
			Admitted: func(tenantID uuid.UUID) []string {
				cfg, err := config.LoadForTenant(context.Background(), st, tenantID)
				if err != nil {
					log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: load tenant config for skill admission check")
					return nil // fail closed: an unreadable config admits nothing
				}
				return cfg.AdmittedSkillIDs
			},
		},
		builtin.ReadSkillFile{Catalog: skillCatalog},
		builtin.Delegate{
			Ledger:   delegations,
			Spawner:  nexusdDelegationSpawner{delegations: delegations},
			Registry: reg,
		},
		builtin.ReadBoard{Resolver: nexusdBoardAdapter{teams: teamsSvc}, Reader: nexusdBoardAdapter{teams: teamsSvc}},
		builtin.ClaimCard{Resolver: nexusdBoardAdapter{teams: teamsSvc}, Claimer: nexusdBoardAdapter{teams: teamsSvc}},
		builtin.WriteCard{Resolver: nexusdBoardAdapter{teams: teamsSvc}, Writer: nexusdBoardAdapter{teams: teamsSvc}},
		builtin.UpdateCardStatus{Resolver: nexusdBoardAdapter{teams: teamsSvc}, Updater: nexusdBoardAdapter{teams: teamsSvc}},
		builtin.ConnectorFetch{Tokens: vault, Sessions: nexusdSessionLookup{store: st}, AllowedHosts: webFetchAllowlist},
		builtin.Retrieve{Searcher: nexusdRetrieverAdapter{retriever: retriever}},
		builtin.AskClarification{},
	}
	// platform/web_crawl (README docs/build-phases.md Phase 16, task 16.1) is
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
	var toolRefs []string
	var catalog []provider.ToolSchema
	for _, t := range builtinTools {
		if err := reg.Register(t); err != nil {
			fatalf("register tool %s: %v", t.ID(), err)
		}
		status, findings := tools.Scan(t.Descriptor())
		if err := reg.SetAdmissionStatus(t.ID(), status); err != nil {
			fatalf("set admission status for %s: %v", t.ID(), err)
		}
		if status != tools.AdmissionClean {
			fatalf("builtin tool %s failed admission (%s): %v", t.ID(), status, findings)
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
	pipeline := tools.NewPipeline(tools.PipelineConfig{
		Registry:         reg,
		Manifest:         manifest,
		Chain:            permChain,
		Hooks:            hooks.NewDispatcher(),
		Blobs:            tools.BlobStore{Dir: envOr("NEXUS_BLOB_DIR", ".dev/blobs")},
		WorkspaceRoot:    workspaceRoot,
		DerivedArtifacts: derivedArtifactRecorder(st, chain),
		SandboxFactory:   newSandboxFactory(workspaceRoot),
		// Claims (README task 6.6): write-ahead idempotency for every
		// non-read-only tool call. Built here (rather than passed in) so
		// newToolPipeline stays the one place that wires the resident
		// catalog — runctl.NewClaimTracker takes st/keyStore/chain
		// directly, not a *runctl.Control, precisely so this can happen
		// before a Control (which needs the *kernel.Kernel this very
		// pipeline feeds into) exists.
		Claims: runctl.NewClaimTracker(st, keyStore, chain),
		// Dynamic (README task 11.1): a ref the static Manifest above
		// doesn't know about — every per-tenant MCP tool, by construction
		// — falls through to mcpResolver instead of failing immediately.
		Dynamic: mcpResolver,
	})
	return pipeline, catalog, manifest.Digest, admittedBundles, mcpPort
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
func newConnectorRegistry() *connectors.Registry {
	names := strings.Split(envOr("NEXUS_OAUTH_PROVIDERS", ""), ",")
	var providers []connectors.Provider
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
		providers = append(providers, connectors.Provider{
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
	return connectors.NewRegistry(providers...)
}

// devSkillsSigningPubkey is the public half of the fixed, deterministic
// ed25519 dev key docs/build-phases.md Phase 16 (task 16.7) signed
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
func loadSkillCatalog(reg *tools.Registry) (*skills.Catalog, []skills.SkillBundle, []tools.Tool) {
	bundles, err := skills.LoadBundles(envOr("NEXUS_SKILLS_ROOT", "skills"))
	if err != nil {
		fatalf("load skill bundles: %v", err)
	}
	if len(bundles) == 0 {
		return skills.NewCatalog(nil), nil, nil
	}

	var pubKey ed25519.PublicKey
	if raw := envOr("NEXUS_SKILLS_SIGNING_PUBKEY", devSkillsSigningPubkey); raw != "" {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			fatalf("decode NEXUS_SKILLS_SIGNING_PUBKEY: %v", err)
		}
		pubKey = ed25519.PublicKey(decoded)
	}

	if err := reg.DeclareNamespace("skill", "nexusd"); err != nil {
		fatalf("declare skill namespace: %v", err)
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
	return skills.NewCatalog(admitted), admitted, scriptTools
}

// derivedArtifactRecorder wires internal/tools.DerivedArtifactRecorder to a
// durable derived_artifacts row (README task 5.4) — best-effort, its own
// small transaction (not the caller's): internal/crypto/shred.go's
// ReconcileDerivedArtifacts is the backstop for whatever this misses, per
// its own doc comment.
func derivedArtifactRecorder(st *store.Store, chain *audit.Chain) tools.DerivedArtifactRecorder {
	_ = chain // reserved: a future phase may want a receipt per spill too; not required by task 5.4 itself
	return func(ctx context.Context, tenantID, sessionID uuid.UUID, kind, path string) error {
		return st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO derived_artifacts (artifact_id, tenant_id, session_id, kind, path) VALUES ($1,$2,$3,$4,$5)`,
				uuid.New(), tenantID, sessionID, kind, path,
			)
			return err
		})
	}
}

// newSandboxFactory wires platform/shell to run inside Docker (README task
// 5.12) when NEXUS_SANDBOX=docker and a daemon is actually reachable, or
// inside an OpenSandbox sandbox (docs/build-phases.md Phase 16, task 16.4)
// when NEXUS_SANDBOX=opensandbox and NEXUS_OPENSANDBOX_URL is configured —
// opt-in, not the default: this demo's zero-setup path (`make up && make
// run`) must keep working on a machine with no Docker daemon and no
// OpenSandbox server running, exactly like NEXUS_PROVIDER=fake needs no
// ANTHROPIC_API_KEY. Both branches share the same fail-open-to-unsandboxed
// posture on a misconfiguration — a demo that asked for isolation and can't
// get it stays running, not fataled, matching the `docker` branch's own
// long-standing behavior.
func newSandboxFactory(workspaceRoot string) func(uuid.UUID) tools.SandboxExec {
	switch envOr("NEXUS_SANDBOX", "") {
	case "docker":
		docker, err := sandbox.NewDocker()
		if err != nil {
			log.Warn().Err(err).Msg("nexusd: NEXUS_SANDBOX=docker but connecting to Docker failed; platform/shell stays unsandboxed")
			return nil
		}
		return func(sessionID uuid.UUID) tools.SandboxExec {
			return sandbox.SessionSandbox{
				Docker: docker,
				Config: sandbox.Config{WorkspaceDir: filepath.Join(workspaceRoot, sessionID.String())},
			}
		}
	case "opensandbox":
		domain := envOr("NEXUS_OPENSANDBOX_URL", "")
		if domain == "" {
			log.Warn().Msg("nexusd: NEXUS_SANDBOX=opensandbox but NEXUS_OPENSANDBOX_URL is unset; platform/shell stays unsandboxed")
			return nil
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
		}
	default:
		return nil
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
	return safety.VerdictDefer, "no real safety model configured (Phase 3 demo default)", nil
}
