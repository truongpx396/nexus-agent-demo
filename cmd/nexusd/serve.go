package main

import (
	"context"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

// runServe is the default command: print the build, then serve until ctx is
// cancelled by SIGTERM or SIGINT, which is what unblocks the HTTP server's
// graceful Shutdown and, in turn, every defer serve registers (README task 13.2,
// F2).
func runServe(ctx context.Context, _ []string) error {
	fmt.Printf("nexusd %s (%s)\n", version.Version, version.GitCommit)
	return serve(ctx)
}

// serve is the process's startup sequence: open the infrastructure, build the
// feature graph over it, start the background loops, and serve HTTP until ctx is
// cancelled. Each step lives in the file named for it (infra.go, compose.go,
// background.go, surfaces_wiring.go, http.go), and its stop is deferred here, so
// shutdown runs the reverse of this order: the HTTP drain, then the loops, then
// the exporters, then the pool.
func serve(ctx context.Context) error {
	cfg, err := loadServerConfig(devMode)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	in, err := openInfra(ctx, cfg)
	if err != nil {
		return err
	}
	defer in.close()

	prov, err := newProvider()
	if err != nil {
		return fmt.Errorf("configure provider: %w", err)
	}

	// The span exporter exists before the kernel does: Kernel.Tracer needs this
	// same exporter, not just runsrest.Handler.Exporter.
	spanExp, shutdownSpanExp, err := newSpanExporter(ctx)
	if err != nil {
		return fmt.Errorf("configure span exporter: %w", err)
	}
	defer shutdownSpanExp()

	stopProfiling, err := startProfiling()
	if err != nil {
		return err
	}
	defer stopProfiling()

	svc, err := buildServices(in, prov, spanExp)
	if err != nil {
		return err
	}
	runsAPI, restAPI := newRESTAPI(in, svc, spanExp)

	stopBackground := startBackground(ctx, in, svc, runsAPI)
	defer stopBackground()

	fmt.Printf("listening on %s (provider=%s)\n", cfg.HTTPAddr, envOr("NEXUS_PROVIDER", "fake"))
	return runHTTPServer(ctx, cfg.HTTPAddr, newMux(in, svc, restAPI))
}
