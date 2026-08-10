# PCVM quality gates

The release test suite always runs every Go package with the race detector. In
addition, PCVM enforces at least 80% aggregate statement coverage over the
security- and lifecycle-critical core. The scope is fixed in
`cmd/core-coverage/main.go` and contains:

- `archive_fs.go`
- `catalog.go`
- `config.go`
- `game_args.go`
- `memory.go`
- `operation.go`
- `receipt.go`
- `reconcile.go`
- `reset.go`
- `runtime_manifest.go`
- `safe_proxy.go`
- `state.go`

The gate is reproducible locally:

```sh
go test -covermode=atomic -coverprofile=coverage.out ./internal/pcvm
go run ./cmd/core-coverage -profile coverage.out -threshold 80
```

The checker fails if any scoped file is absent from the profile, so a rename or
scope change must be reviewed explicitly rather than silently lowering the
coverage denominator.

Provider coverage is a separate contract gate. The explicit 53-provider matrix
in `platform_contract_matrix_test.go` binds every release ID to compiled
resolver, installer, process and version drivers, validates runtime,
transition, rollback and preservation metadata, and constructs representative
launch plans without network access. Live upstream checks remain confined to
the nightly workflow.
