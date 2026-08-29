`boundaries_test.go` (Phase 1) pins the import-boundary rules README.md §4
declares, using `go/packages` to walk the real import graph. Rules naming a
package that doesn't exist yet (`kernel/`, `internal/surfaces`,
`internal/controlplane` — Phase 2/7) report `SKIP` until that phase lands;
they are not silently vacuous — the test fails if it ever finds zero
existing packages to check.

The kernel ABI, the control-plane <-> data-plane `v1` shapes, and the
run-API OpenAPI surface get their own contract tests alongside the phases
that introduce them (Phase 2, 2, 7 respectively).

`cost_metering_test.go` (Phase 4) pins README.md §5's own mitigation for
"cost metering gets bolted onto foreground turns only": an AST check, using
`go/packages` with full type information, that finds every call resolving
to `internal/provider.Provider.Stream` and requires a `Reserve` call
earlier in the same enclosing function. Like `boundaries_test.go`, it fails
loudly (`checked == 0`) rather than silently if it ever stops finding a
real call site to check.
