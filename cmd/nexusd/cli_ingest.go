package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/ingest"
	"github.com/truongpx396/nexus-agent-demo/internal/retrieval"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// runIngest is Phase 12's admin operation (README §5's Demo: "ingest a
// 200-page PDF"): convert, admission-scan, embed, and index one local file
// for a tenant — the same one-shot, construct-everything-from-scratch shape
// runErase above already has, because ingestion (like erasure) is an
// operator action outside any kernel session, not a tool a model calls
// (platform/retrieve, the tool a model DOES call, only ever searches an
// already-ingested corpus). It still mints and persists a minimal session
// row below purely as a cost-accounting label — see that INSERT's own
// comment for why a real row is required, not optional, here.
func runIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	tenantName := fs.String("tenant", "", "tenant name to ingest into (required)")
	path := fs.String("file", "", "path to the local source file to ingest (required)")
	mimeType := fs.String("mime", "", fmt.Sprintf("mime type: one of %s, %s, %s, %s (required)",
		ingest.MimePlainText, ingest.MimeHTML, ingest.MimePDF, ingest.MimeDOCX))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantName == "" || *path == "" || *mimeType == "" {
		return fmt.Errorf("--tenant, --file, and --mime are all required")
	}

	data, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("read %s: %w", *path, err)
	}

	dsn := envOr("NEXUS_DATABASE_URL", defaultAppDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	st := store.New(pool)

	redisClient := redis.NewClient(&redis.Options{Addr: envOr("NEXUS_REDIS_ADDR", defaultRedisAddr)})
	gate := cost.NewGate(st, redisClient, cost.DefaultMeters(), cost.GateConfig{})
	retriever := &retrieval.Retriever{Store: st, Gate: gate, Embedder: newEmbedder(), ModelID: "fake-embedder-v1"}

	tenantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("nexus-agent-demo/tenant/"+*tenantName))
	sessionID := uuid.New()

	// cost_records.session_id and budget_decisions.session_id are both
	// NOT NULL REFERENCES sessions(session_id) (migrations/0005_cost.sql)
	// — every Reserve/Reconcile call, ingestion's own embedding calls
	// included, needs a REAL session row to attribute cost to, even one
	// with no kernel run behind it. This is the one place `nexusd ingest`
	// differs from `nexusd erase`: erasure never reserves a model call, so
	// it never hits this requirement.
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "ingest-cli", UserID: uuid.Nil, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: []byte{0},
		})
	})
	if err != nil {
		return fmt.Errorf("create ingestion session: %w", err)
	}

	doc, err := retriever.IndexDocument(ctx, tenantID, sessionID, filepath.Base(*path), *mimeType, data)
	if err != nil {
		return fmt.Errorf("ingest %s: %w", *path, err)
	}
	fmt.Printf("ingested %q into tenant %q: doc_id=%s admission_status=%s chunk_count=%d\n",
		doc.SourceName, *tenantName, doc.DocID, doc.AdmissionStatus, doc.ChunkCount)
	if doc.AdmissionStatus != "clean" {
		fmt.Printf("  fail-closed: nothing was indexed (findings: %v)\n", doc.AdmissionFindings)
	}
	return nil
}
