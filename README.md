pg-dumpling
===========

`pg-dumpling` is a fork of [`dumpling`](https://github.com/pingcap/tidb/tree/master/dumpling)
extracted from the TiDB monorepo, with the long-term goal of supporting
PostgreSQL as a source database in addition to MySQL/TiDB.

This is the **Phase 0 baseline**: dumpling code physically lifted out of the
TiDB tree and made buildable as a standalone Go module. No behavior changes
from upstream yet.

Status
------

- Module path: `github.com/tadapin/pg-dumpling`
- Source baseline: snapshot of `~/Work/tidb/dumpling` at TiDB commit
  `ae18096e02378` (2026-04-26)
- TiDB-internal dependencies (`github.com/pingcap/tidb/{br,pkg}/...`) are
  resolved via a local `replace` to `/Users/bohnen/Work/tidb` — this repo
  cannot build on a machine without that checkout. Phase 1 will start
  vendoring or replacing those dependencies.

Prerequisites
-------------

- Go 1.25.8 or newer (the build will auto-fetch the toolchain via
  `GOTOOLCHAIN=auto` if your installed `go` is older).
- A local TiDB checkout at `/Users/bohnen/Work/tidb`. If yours is elsewhere,
  edit the `replace` directive in `go.mod`.

Building
--------

```sh
make build
./bin/dumpling --version
./bin/dumpling --help
```

Tests
-----

```sh
make test-unit              # short unit tests, no failpoint rewrite
make test-unit-failpoint    # full unit tests after rewriting failpoints
```

A few tests in `export/` (`TestWriteTableMeta`, `TestWriteTableData`)
intentionally rely on `failpoint`-injected error paths and only pass when the
sources have been rewritten by `failpoint-ctl`. Install it once with:

```sh
go install github.com/pingcap/failpoint/failpoint-ctl@latest
```

The integration suite under `tests/` is shell-driven and requires TiDB,
MinIO, and other services. It is not wired into the Makefile in this baseline.

Layout
------

```
cli/             version metadata
cmd/dumpling/    CLI entry point (main package)
context/         logger-aware context wrapper
export/          dump engine (config, dump loop, sql gen, writer, ir, ...)
log/             zap-based logging facade
tests/           upstream integration tests (not yet runnable here)
docs/            upstream user docs (en/cn)
```

License
-------

Apache 2.0, inherited from upstream `pingcap/tidb`. See `LICENSE`.
