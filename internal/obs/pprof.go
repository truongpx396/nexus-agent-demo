package obs

import (
	"context"
	"errors"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"github.com/rs/zerolog/log"
)

// StartPprofServer exposes Go's net/http/pprof handlers (docs/observability.md)
// on their OWN listener, bound to addr — never on the mux
// cmd/nexusd/serve.go's REST/webhook surface or /metrics already share.
// net/http/pprof's own package doc warns against mounting it on a mux
// anything untrusted can reach: /debug/pprof/profile lets an unauthenticated
// caller pin a CPU core for up to `seconds` (30 by default, caller-settable
// via a query param) with no auth gate, and /debug/pprof/heap dumps live
// allocation stacks — strictly more sensitive than the "unauthenticated,
// content-free, per-process" caveat docs/observability.md already documents
// for /metrics, so this never shares that caveat's mux either. Operators are
// expected to bind addr to loopback (127.0.0.1:6060) or a network the public
// internet can't reach, the same "network-level restriction" docs/
// observability.md's own /metrics caveat recommends.
//
// addr == "" disables this entirely and returns a no-op shutdown — the
// zero-setup default every other optional adapter in this codebase follows
// (NEXUS_OTLP_ENDPOINT unset means no exporter dialed; NEXUS_SANDBOX unset
// means platform/shell runs unsandboxed).
func StartPprofServer(addr string) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if addr == "" {
		return noop, nil
	}

	// Registered explicitly on a fresh mux rather than importing net/http/pprof
	// for its side-effecting init() (which would register onto
	// http.DefaultServeMux) — this codebase never touches DefaultServeMux
	// anywhere else, and a side-effect import here would be an invisible
	// dependency between this file and whichever mux happens to be the
	// default at process start.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Unbounded, like cmd/nexusd/serve.go's own main httpSrv.WriteTimeout:
		// /debug/pprof/profile and /debug/pprof/trace both hold the response
		// open for the caller's requested duration, so any fixed timeout here
		// would sever a legitimate long capture.
		WriteTimeout: 0,
	}

	go func() {
		if lerr := srv.ListenAndServe(); lerr != nil && !errors.Is(lerr, http.ErrServerClosed) {
			log.Error().Err(lerr).Str("addr", addr).Msg("obs: pprof server")
		}
	}()
	log.Info().Str("addr", addr).Msg("obs: pprof server listening (bind to loopback or another trusted-only network)")

	return srv.Shutdown, nil
}

// EnableMutexBlockProfiling turns on Go's mutex-contention and goroutine-
// blocking samplers. Both are off by default (runtime's own zero value) and
// stay off unless explicitly asked for here, because both carry sampling
// overhead that applies process-wide the instant they're turned on — even
// before anything ever reads /debug/pprof/mutex|block or Pyroscope's
// mutex_count/block_count/mutex_duration/block_duration profile types.
//
// This is the ONE call that makes either of those consumers' mutex/block
// output non-empty: github.com/grafana/pyroscope-go never calls either
// runtime setter itself (confirmed against its session.go — it only reads
// whatever profile runtime/pprof already has queued, it never turns
// collection on), and net/http/pprof's own docs carry the identical
// warning for /debug/pprof/mutex and /debug/pprof/block. One call here
// benefits both consumers at once, since they read the same underlying
// runtime profile.
//
// mutexFraction is runtime.SetMutexProfileFraction's own parameter ("report
// 1 in N contended mutex unlock events"); blockRate is
// runtime.SetBlockProfileRate's own parameter ("report 1 in N nanoseconds
// of blocking"), same units and semantics the stdlib functions themselves
// define. 0 (this package's default) leaves the matching sampler off.
func EnableMutexBlockProfiling(mutexFraction, blockRate int) {
	if mutexFraction > 0 {
		runtime.SetMutexProfileFraction(mutexFraction)
	}
	if blockRate > 0 {
		runtime.SetBlockProfileRate(blockRate)
	}
}
