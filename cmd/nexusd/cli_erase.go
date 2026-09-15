package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/retrieval"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// runErase is `nexusd erase --tenant=name` (or --session=<uuid>): the
// admin operation behind internal/crypto/shred.go's erasure transaction
// (README task 5.4) — destroys the DEK(s), hard-deletes derived artifacts,
// and appends an EventErasure per affected session.
func runErase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("erase", flag.ExitOnError)
	tenantName := fs.String("tenant", "", "tenant name to erase (mutually exclusive with --session)")
	sessionArg := fs.String("session", "", "session id to erase (mutually exclusive with --tenant)")
	reason := fs.String("reason", "operator-requested erasure", "reason recorded on the EventErasure record")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*tenantName == "") == (*sessionArg == "") {
		return fmt.Errorf("exactly one of --tenant or --session is required")
	}

	dsn := envOr("NEXUS_DATABASE_URL", defaultAppDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	st := store.New(pool)

	kek, err := loadOrGenerateKEK(envOr("NEXUS_KEK_PATH", defaultKEKPath), devMode)
	if err != nil {
		return fmt.Errorf("load KEK: %w", err)
	}
	_ = crypto.NewKeyStore(kek) // erasure only shreds/reads encryption_keys rows directly; KeyStore isn't needed for that path itself

	signer := audit.NewSignerClient(envOr("NEXUS_SIGNERD_SOCKET", defaultSignerdSocket))
	chain := audit.NewChain(signer)

	if *tenantName != "" {
		tenantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("nexus-agent-demo/tenant/"+*tenantName))
		var result crypto.ErasureResult
		var deletedChunks, deletedDocs int
		err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var eerr error
			result, eerr = crypto.EraseTenant(ctx, tx, chain, tenantID, *reason)
			if eerr != nil {
				return eerr
			}
			// Phase 12, task 12.8: the retrieval index is a second durable,
			// tenant-owned knowledge store outside the encrypted event log
			// (like derived_artifacts, but not session-scoped — see
			// internal/retrieval.DeleteTenant's own doc comment) — it must
			// empty in the SAME erasure transaction as the DEK shred above,
			// never on a best-effort follow-up job.
			deletedChunks, deletedDocs, eerr = retrieval.Erase(ctx, tx, tenantID)
			return eerr
		})
		if err != nil {
			return fmt.Errorf("erase tenant %s: %w", *tenantName, err)
		}
		reclaimArtifacts(result)
		fmt.Printf("erased tenant %q: %d key(s) shredded, %d session(s), %d derived artifact(s) removed, %d retrieval chunk(s) and %d document(s) removed\n",
			*tenantName, len(result.ShreddedKeyIDs), len(result.ErasureEvents), len(result.DeletedArtifacts), deletedChunks, deletedDocs)
		return nil
	}

	sessionID, err := uuid.Parse(*sessionArg)
	if err != nil {
		return fmt.Errorf("invalid --session: %w", err)
	}
	// A session's tenant isn't known up front from the id alone — and pool
	// connects as nexus_app, RLS-restricted to whatever app.tenant_id the
	// CURRENT transaction set, which is nothing yet (same reason
	// listTenantIDs above connects as the admin superuser instead of
	// through st): resolving "which tenant owns this session" is
	// necessarily a cross-tenant admin lookup, done here the same way.
	adminDSN := envOr("NEXUS_ADMIN_DATABASE_URL", envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN))
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect as admin: %w", err)
	}
	defer adminPool.Close()
	var tenantID uuid.UUID
	if err := adminPool.QueryRow(ctx, `SELECT tenant_id FROM sessions WHERE session_id = $1`, sessionID).Scan(&tenantID); err != nil {
		return fmt.Errorf("resolve tenant for session %s: %w", sessionID, err)
	}
	var result crypto.ErasureResult
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var eerr error
		result, eerr = crypto.EraseSession(ctx, tx, chain, tenantID, sessionID, *reason)
		return eerr
	})
	if err != nil {
		return fmt.Errorf("erase session %s: %w", sessionID, err)
	}
	reclaimArtifacts(result)
	fmt.Printf("erased session %s: %d key(s) shredded, %d derived artifact(s) removed\n", sessionID, len(result.ShreddedKeyIDs), len(result.DeletedArtifacts))
	return nil
}

// reclaimArtifacts best-effort unlinks the files EraseTenant/EraseSession
// already hard-deleted the derived_artifacts ROWS for — file removal can't
// be transactional with Postgres, so a failure here is logged, not fatal:
// crypto.ReconcileDerivedArtifacts is the backstop for exactly this case.
func reclaimArtifacts(result crypto.ErasureResult) {
	for _, a := range result.DeletedArtifacts {
		if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
			log.Warn().Err(err).Str("path", a.Path).Msg("nexusd: failed to unlink derived artifact after erasure")
		}
	}
}
