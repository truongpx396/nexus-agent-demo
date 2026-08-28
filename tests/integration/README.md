The Phase 1 tenant-isolation test — through PgBouncer in transaction-pooling
mode, the load-bearing test named in README.md §5 (task 1.4) — lives at
`internal/store/isolation_integration_test.go` instead of here: it needs to
call an unexported helper (`scopeTenant`) to construct the session-level
scoping variant Principle VI forbids, which only a white-box test inside
`package store` can do.

This directory holds **black-box** integration tests — ones that exercise
more than one package's public API together and don't need to reach into
either one's internals. The first candidates land with Phase 2 (a run
through the REST surface end to end) and Phase 6 (resume-from-checkpoint,
concurrent cost-ceiling enforcement). All integration tests, wherever they
live, build under the `integration` tag and run via
`go test -tags=integration ./...`.
