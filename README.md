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
- **Phase 2** (`v0.4.0`) — PG → MySQL/TiDB migration via CSV. Server-
  side casts in CSV mode emit PG-specific types (timestamptz, bytea,
  uuid, jsonb, arrays, ranges, enum, interval, inet) in a form TiDB
  Lightning / `LOAD DATA LOCAL INFILE` can ingest. Table-internal
  chunking restored (numeric PK range + ctid fallback). TiDB-internal
  imports trimmed from 7 to 1; only `br/pkg/storage` (cloud backend
  layer) is still pulled from `pingcap/tidb`. Verified end-to-end
  against `postgres:17` ↔ `tidb:v8.5.6` and `mysql:8.4`. See
  `worklog/04-phase2-roadmap.md`.
- **Phase 3a** (`v0.5.0`) — `replace ~/Work/tidb` removed. The remaining
  `br/pkg/storage` import is now sourced from a public commit pin
  (`v1.1.0-beta.0.20260413061245-ae18096e0237` = TiDB v8.5.6) via
  `proxy.golang.org`; no local TiDB checkout is required to build.
  See `worklog/05-phase3a-public-commit-pin.md`.
- **Phase 3** (`v0.6.0`) — SQL output is now MySQL/TiDB-targeted by
  default. A new `--target {mysql,tidb,pg}` flag selects the SQL flavor
  (backtick identifiers, MySQL preamble, `X'…'` bytea, server-side
  casts). With the default `--target=mysql`, dump files can be loaded
  directly via `mysql -h … < dump.sql` against MySQL 8.4 / TiDB v8.5.6.
  Use `--target=pg` for the legacy PG round-trip behavior. See
  `worklog/06-phase3-mysql-sql.md`.
- **Phase 4b** (`v0.8.0`) — CDC bootstrap for AWS DMS / logical
  decoding. `--cdc-slot <name>` atomically creates a logical
  replication slot (`pgoutput` / `test_decoding` /
  `pglogical_output`) via `CREATE_REPLICATION_SLOT … EXPORT_SNAPSHOT`,
  ties it to the dump's MVCC snapshot, and writes the slot name +
  consistent_point LSN to the `metadata` file so a CDC consumer can
  resume from exactly the dump's point. See
  `worklog/08-phase4b-cdc-bootstrap.md`.
- **Phase 4c** (`v0.9.0`) — `--no-preamble` flag. SQL output files
  normally start with a SET-block (`SET sql_mode='NO_BACKSLASH_ESCAPES'`
  etc.) so they can be loaded directly via `mysql -h … <`. TiDB
  Lightning rejects bare SET statements, so `--target=tidb` now
  defaults to no-preamble. Override either way with `--no-preamble`
  / `--no-preamble=false`.

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
- No local TiDB checkout is required since v0.5.0. The remaining
  `github.com/pingcap/tidb/br/pkg/storage` import is pinned to a public
  pseudo-version of v8.5.6 in `go.mod` and resolved via
  `proxy.golang.org`. To bump it, run `go get
  github.com/pingcap/tidb@<commit-or-tag>` followed by `go mod tidy`.

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

Loading into MySQL/TiDB (the default target since v0.6.0):

```sh
# 1. Translate the PG DDL into MySQL/TiDB DDL (LLM-assisted is fine).
mysql -h <target> -e "CREATE DATABASE demo"
mysql -h <target> demo < hand_translated_schema.sql

# 2. The data files default to --target=mysql output.
./bin/pg-dumpling -h pg -B demo -o /tmp/dump --filetype sql
mysql -h <target> < /tmp/dump/public.t.000000000.sql
```

For larger datasets `--filetype csv` is also fully MySQL/TiDB-compatible
and ingests well via TiDB Lightning or `LOAD DATA LOCAL INFILE`. See
`worklog/04-phase2-roadmap.md` for the type-cast matrix and
`worklog/06-phase3-mysql-sql.md` for the SQL-target details.

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
   --target         mysql | tidb | pg   (SQL-output dialect, default mysql)
   --no-preamble    suppress the SET-block at the top of SQL files
                    (default ON for --target=tidb, OFF otherwise)
   --consistency    auto | snapshot | none
   --cdc-slot       create a logical replication slot atomically with the
                    snapshot (for AWS DMS / pg_recvlogical to resume CDC)
   --cdc-plugin     pgoutput | test_decoding | pglogical_output (default pgoutput)
   --cdc-cleanup-on-failure
                    drop the slot if the dump fails (default true)
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
