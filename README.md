pg-dumpling
===========

`pg-dumpling` is a PostgreSQL data dump tool that emits the
[dumpling](https://github.com/pingcap/tidb/tree/master/dumpling) directory
format. It is designed as a building block for **PostgreSQL → MySQL / TiDB
data migration**: data files use SQL standard literals so that, with the
right preamble and a target schema, the rows can be replayed into a
MySQL-family database.

Status
------

- **Phase 0** (`v0.1.0-baseline`) — dumpling extracted from the TiDB
  monorepo as a standalone Go module.
- **Phase 1** (`v0.3.0-postgres`) — full PostgreSQL source path. The
  binary connects via `pgx`, queries `pg_catalog`, snapshots via
  `pg_export_snapshot()`, shells out to `pg_dump --schema-only` for DDL,
  and writes per-table data files in dumpling's chunked layout.
- **Phase 2** (in progress) — PG → MySQL/TiDB migration via CSV.
  Hardening of the CSV path so that PG-specific types (timestamptz,
  bytea, uuid, jsonb, arrays, ranges, enum, interval, inet) are
  emitted in a form TiDB Lightning can ingest, plus revival of
  table-internal chunking and removal of the local TiDB `replace`
  directive. Direct MySQL-loadable SQL output is **not** pursued —
  the PG/MySQL incompatibility surface is too wide for SQL-level
  translation. See `worklog/04-phase2-roadmap.md`.

What gets dumped
----------------

For each PostgreSQL schema selected by `--filter`:

- `<schema>-schema-create.sql` — `CREATE SCHEMA IF NOT EXISTS "schema"`
- `<schema>.<table>-schema.sql` — verbatim output of
  `pg_dump --schema-only --no-owner --no-privileges -t schema.table`.
  The DDL is **PostgreSQL-native**; converting it for a MySQL/TiDB
  target is left to the user (LLMs are well-suited to this).
- `<schema>.<table>.<n>.sql` (or `.csv`) — chunked data files. SQL files
  contain `INSERT INTO "schema"."table" VALUES (...)` statements with
  PG-standard escaping; the file is preceded by a `SET
  standard_conforming_strings = on; SET client_encoding = 'UTF8'; SET
  search_path = pg_catalog;` preamble.
- `metadata` — currently a no-op for PostgreSQL (Phase 2 will add
  `pg_current_wal_lsn()`).

The orchestration is identical to upstream dumpling: `--threads` worker
pool, file-size rolling, optional `gzip`/`snappy`/`zstd` compression,
external storage targets (S3/GCS/Azure), Prometheus metrics, an HTTP
status endpoint.

Prerequisites
-------------

- **Go 1.25.8 or newer** (the build will auto-fetch the toolchain via
  `GOTOOLCHAIN=auto` if your installed `go` is older).
- **`pg_dump`** on `$PATH` of the machine running pg-dumpling. Schema
  files are produced by spawning `pg_dump --schema-only` against the
  source. The bundled child uses the same connection parameters as the
  data path. A version of `pg_dump` that matches or exceeds the source
  server is recommended.
- A local TiDB checkout at `/Users/bohnen/Work/tidb` is still required
  for module resolution (the few remaining `br/pkg/...` and
  `pkg/util/...` imports). Phase 2 will vendor or rewrite those so the
  `replace` directive can be removed.

Building
--------

```sh
make build
./bin/pg-dumpling --help
```

Quick start
-----------

```sh
docker run --rm -d --name pg -e POSTGRES_PASSWORD=secret -p 5433:5432 postgres:17
psql -h 127.0.0.1 -p 5433 -U postgres -c "CREATE DATABASE demo"
psql -h 127.0.0.1 -p 5433 -U postgres -d demo -c \
  "CREATE TABLE t(id int primary key, s text);
   INSERT INTO t SELECT g, 'row '||g FROM generate_series(1,1000) g"

./bin/pg-dumpling \
    -h 127.0.0.1 -P 5433 -u postgres -p secret \
    -B demo -o /tmp/dump --threads 4 --consistency snapshot

ls /tmp/dump
# metadata
# public-schema-create.sql
# public.t-schema.sql
# public.t.000000000.sql
```

Reload into a fresh PostgreSQL:

```sh
psql -h 127.0.0.1 -p 5433 -U postgres -c "CREATE DATABASE demo2"
psql ... -d demo2 -f /tmp/dump/public.t-schema.sql
psql ... -d demo2 -f /tmp/dump/public.t.000000000.sql
```

Loading into MySQL/TiDB is **not** a direct `psql -f` operation — the
data files use PG-flavored literals and the schema is PG-native. The
Phase 2 plan is:

1. Translate the `-schema.sql` files (PG DDL) into MySQL/TiDB DDL
   yourself or via an LLM, then run them on the target.
2. Re-run pg-dumpling with `--filetype csv` (Phase 2 hardens this for
   PG-specific types via server-side casts) and feed the resulting
   files to TiDB Lightning.

See `worklog/04-phase2-roadmap.md` for the type-cast matrix and the
implementation roadmap.

Flag highlights
---------------

```
-h --host           PostgreSQL host (default 127.0.0.1)
-P --port           PostgreSQL port (default 5432)
-u --user           PostgreSQL user (default postgres)
-p --password       password
-B --database       database name (PGDATABASE for the connection)
-o --output         output directory
-t --threads        worker count
   --filetype       sql | csv
   --consistency    auto | snapshot | none
   --snapshot       reuse a pg_export_snapshot() token
-f --filter         table filter glob, e.g. "public.*" or "!pg_catalog.*"
   --no-schemas     skip emitting -schema.sql files
   --no-data        emit only schema files
   --no-views       skip views (default true)
   --compress       gzip | snappy | zstd | no-compression
   --rows           split tables into N-row chunks (Phase 2; currently no-op)
```

`--allow-cleartext-passwords`, `--tidb-mem-quota-query`, and the
charset/collation flags from upstream dumpling are removed.

Tests
-----

```sh
make test-unit              # short unit tests, no failpoint rewrite
make test-unit-failpoint    # full unit tests after rewriting failpoints
```

Most upstream MySQL-specific tests were dropped in Phase 1; PG-specific
unit tests are pending and will be added in Phase 2 alongside the
TiDB-target work.

Layout
------

```
cli/             version metadata
cmd/pg-dumpling/ CLI entry point
context/         logger-aware context
export/          dump engine
log/             zap wrapper
worklog/         per-step engineering notes (00 baseline, 01 plan,
                 02 strip, 03 pg-source, 04 phase 2 roadmap)
```

License
-------

Apache 2.0, inherited from upstream `pingcap/tidb`. See `LICENSE`.
