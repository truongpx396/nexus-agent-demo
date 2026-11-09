`boundaries_test.go` (Phase 1) pins the import-boundary rules README.md §3
declares, using `go/packages` to walk the real import graph. Rules naming a
package that doesn't exist yet (`kernel/`, `internal/surfaces`,
`internal/controlplane` — Phase 2/7) report `SKIP` until that phase lands;
they are not silently vacuous — the test fails if it ever finds zero
existing packages to check.

The kernel ABI, the control-plane <-> data-plane `v1` shapes, and the
run-API OpenAPI surface get their own contract tests alongside the phases
that introduce them (Phase 2, 2, 7 respectively).

`filesize_test.go` pins the repo's file-size convention (CLAUDE.md
Conventions) mechanically: no non-test, non-generated `.go` file may exceed
`maxGoFileLines` (500). It follows the same rule as the checks below — fail
loudly rather than pass vacuously (`checked == 0`) — and carries its own
fixture test proving the scanner can actually flag an oversized file.
`lineCapExempt` is empty on purpose: split the file by responsibility within
its package instead of listing it.

`cost_metering_test.go` (Phase 4) pins the risk register's own mitigation (the end of `specs/001-agent-platform/plan.md`) for
"cost metering gets bolted onto foreground turns only": an AST check, using
`go/packages` with full type information, that finds every call resolving
to `internal/provider.Provider.Stream` and requires a `Reserve` call
earlier in the same enclosing function. Like `boundaries_test.go`, it fails
loudly (`checked == 0`) rather than silently if it ever stops finding a
real call site to check.

`goleak_test.go` pins the goroutine-leak rule mechanically: every package whose
non-test code starts a goroutine (a `go` statement, found by parsing, not by
grepping) and that has tests must declare a `TestMain` that calls into
`go.uber.org/goleak`, so a test that leaves a goroutine running fails its
package. A `TestMain` that never touches goleak does not count, and a package
with no tests has nothing to attach one to. Like the checks above it fails
loudly (`checked == 0`) and carries a fixture test proving each way of missing
the check is seen. `leakCheckExempt` is empty on purpose: the fix is a one-line
`leak_test.go`, and for a leaking test a `t.Cleanup` that cancels the context or
closes the client.
