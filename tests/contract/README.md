`boundaries_test.go` (Phase 1) pins the import-boundary rules README.md §4
declares, using `go/packages` to walk the real import graph. Rules naming a
package that doesn't exist yet (`kernel/`, `internal/surfaces`,
`internal/controlplane` — Phase 2/7) report `SKIP` until that phase lands;
they are not silently vacuous — the test fails if it ever finds zero
existing packages to check.

The kernel ABI, the control-plane <-> data-plane `v1` shapes, and the
run-API OpenAPI surface get their own contract tests alongside the phases
that introduce them (Phase 2, 2, 7 respectively).
