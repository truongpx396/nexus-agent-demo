Integration tests land alongside the feature they exercise, starting with
the tenant-isolation test run *through* PgBouncer in transaction-pooling
mode (README.md §5, Phase 1 task 1.4) — the test that makes the
transaction-local RLS claim real rather than assumed.
