pg-dumpling
===========

[![CI](https://github.com/bohnen/pg-dumpling/actions/workflows/ci.yml/badge.svg)](https://github.com/bohnen/pg-dumpling/actions/workflows/ci.yml)

A PostgreSQL data-dump tool that emits the
[dumpling](https://github.com/pingcap/tidb/tree/master/dumpling)
directory layout. Designed as a building block for
**PostgreSQL → MySQL / TiDB data migration**: with a target schema in
place, the dumped files can be replayed via `mysql -h … < dump.sql`,
TiDB Lightning, or `LOAD DATA LOCAL INFILE`.

It can also bootstrap a logical replication slot atomically with the
dump's MVCC snapshot, so a CDC consumer such as AWS DMS can resume from
exactly the dump's point.

Differences from upstream `pingcap/dumpling`
--------------------------------------------

`pg-dumpling` is a fork of the [dumpling](https://github.com/pingcap/tidb/tree/master/dumpling)
tool from the TiDB project. The orchestration and CLI surface are
intentionally close to the original; the data path is rewritten for
PostgreSQL.

| | upstream `dumpling` | `pg-dumpling` |
|---|---|---|
| Source | MySQL / TiDB | PostgreSQL only |
| Driver | `go-sql-driver/mysql` | `jackc/pgx/v5` |
| Catalog discovery | `INFORMATION_SCHEMA` (MySQL) | `pg_catalog` + `information_schema` |
| Snapshot | `START TRANSACTION WITH CONSISTENT SNAPSHOT` / `FLUSH TABLES WITH READ LOCK` | `pg_export_snapshot()` + `SET TRANSACTION SNAPSHOT` |
| DDL | `SHOW CREATE TABLE` | `pg_dump --schema-only -t` (child process) |
| Chunking | row count via `INFORMATION_SCHEMA`; `_tidb_rowid` / PK ranges | `pg_class.reltuples`; numeric PK ranges with `ctid` fallback |
| Targets | MySQL / TiDB | MySQL / TiDB / PG round-trip; CDC handoff |
| `<schema>-schema-create.sql` | `CREATE DATABASE` | `CREATE SCHEMA IF NOT EXISTS` (PG) / `CREATE DATABASE IF NOT EXISTS` (MySQL/TiDB target) |
| Cloud backends | S3 / GCS / Azure (via `br/pkg/storage`) | unchanged |

What is **kept** from upstream:

- `--threads` worker pool, table fan-out, file rolling, CSV/SQL modes
- compression (`gzip`/`snappy`/`zstd`)
- external storage targets (S3 / GCS / Azure / local) via `br/pkg/storage`
- Prometheus metrics, HTTP status endpoint
- file-naming convention(`<db>.<table>.NNNNNNNNN.<ext>` etc.)

Install
-------

Pre-built binaries are attached to each
[GitHub Release](https://github.com/bohnen/pg-dumpling/releases) (raw
binary, `tar.gz`, and `sha256` checksum):

```
pg-dumpling-vX.Y.Z-linux-amd64
pg-dumpling-vX.Y.Z-linux-arm64
pg-dumpling-vX.Y.Z-darwin-arm64    (Apple Silicon)
```

Linux binaries are static (`CGO_ENABLED=0`). The macOS binary is a
native ARM64 build and is **not notarized**; download via `curl` to
avoid Gatekeeper quarantine, or remove the attribute manually:

```sh
# Download via curl (no quarantine attribute)
curl -LO https://github.com/bohnen/pg-dumpling/releases/download/v0.12.0/pg-dumpling-v0.12.0-darwin-arm64
chmod +x pg-dumpling-v0.12.0-darwin-arm64

# Or, after a Safari/Finder download:
xattr -d com.apple.quarantine pg-dumpling-v0.12.0-darwin-arm64
```

To build from source:

```sh
make build              # → bin/pg-dumpling
./bin/pg-dumpling --help
```

Requires **Go 1.25.8 or newer** (the build will auto-fetch the toolchain
via `GOTOOLCHAIN=auto` if your installed `go` is older), and **`pg_dump`**
on `$PATH` (used by a child process to emit `<table>-schema.sql` files).

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
    -B demo -o /tmp/dump --threads 4

ls /tmp/dump
# metadata
# public-schema-create.sql
# public.t-schema.sql
# public.t.000000000.sql
```

Loading the result:

```sh
# MySQL / TiDB (default --target=mysql; the SQL file is self-contained):
mysql -h <target> -e "CREATE DATABASE demo"
mysql -h <target> demo < hand_translated_schema.sql      # translate the PG DDL once
mysql -h <target> < /tmp/dump/public.t.000000000.sql

# TiDB Lightning: use --target=tidb (no preamble) or --filetype=csv
./bin/pg-dumpling -h pg -B demo -o /tmp/dump --target=tidb --threads 8 --rows 200000
tidb-lightning -d /tmp/dump --backend tidb -tidb-host tidb -tidb-port 4000 -tidb-user root

# PostgreSQL round-trip:
./bin/pg-dumpling -h pg -B demo -o /tmp/dump --target=pg
psql ... -d demo2 -f /tmp/dump/public.t-schema.sql
psql ... -d demo2 -f /tmp/dump/public.t.000000000.sql
```

Output layout
-------------

For each PostgreSQL schema selected by `--filter`:

| File | Content |
|---|---|
| `<schema>-schema-create.sql` | `CREATE SCHEMA …` (target=pg) or `` CREATE DATABASE IF NOT EXISTS `…` `` (target=mysql/tidb) |
| `<schema>.<table>-schema.sql` | Verbatim `pg_dump --schema-only -t schema.table`. The DDL stays **PostgreSQL-native**; translate it to MySQL/TiDB DDL yourself (LLMs do this well) |
| `<schema>.<table>.NNNNNNNNN.sql` (or `.csv`) | Chunked data files. SQL files are `INSERT INTO …` statements; CSV files have an optional header row |
| `metadata` | Dump start/finish timestamps. With `--cdc-slot`, also slot name + plugin + consistent_point LSN + snapshot name |

Supported PostgreSQL types
--------------------------

35+ built-in and extension types are handled via server-side casts in
the `SELECT` projection (`pgMigrationCast()` in `export/sql.go`). The
target system controls the cast behavior:

- `--target=mysql` / `--target=tidb`: every non-trivial type is cast to
  a MySQL/TiDB-friendly text form before reaching the writer
- `--target=pg`: native PG literals are emitted (round-trip)
- `--filetype=csv`: casts always run (so the CSV cells stay ASCII-safe);
  bytea is `encode(_, 'hex')` for `LOAD DATA … (@v) SET col = UNHEX(@v)`

| PG type | `--target=mysql/tidb` (SQL/CSV) | `--target=pg` | Recommended target column |
|---|---|---|---|
| `int2/4/8`, `float4/8`, `numeric`, `money`, `oid/xid/cid/tid` | unquoted numeric literal | same | `INT` / `BIGINT` / `DECIMAL` / `DOUBLE` |
| `boolean` | `0` / `1` (cast to int) | `t` / `f` | `TINYINT(1)` |
| `bit(n)`, `bit varying` | `'10101010'` (text) | same | `VARCHAR` |
| `text`, `varchar`, `char`, `bpchar`, `name` | `'…'` SQL-standard escaped | same | `TEXT` / `VARCHAR` |
| `bytea` | SQL: `X'48656C6C6F'`. CSV: hex string + `UNHEX` on loader | `'\x48656c6c6f'` | `VARBINARY` / `BLOB` |
| `timestamp`, `timestamptz` | `'YYYY-MM-DD HH:MI:SS.US'` (UTC for `tz`) via `to_char` | native | `DATETIME(6)` |
| `date` | `'YYYY-MM-DD'` | native | `DATE` |
| `time`, `timetz` | `'HH:MI:SS.US'` | native | `TIME(6)` |
| `interval` | `EXTRACT(EPOCH FROM _)::numeric` (seconds) | native | `DECIMAL(20,6)` |
| `uuid` | `'a0eebc99-…'` (text) | same | `CHAR(36)` |
| `json`, `jsonb`, `xml` | `'{…}'` (text) | same | `JSON` (or `LONGTEXT`) |
| `inet`, `cidr` | `'192.168.1.10/32'` (text) | native | `VARCHAR` |
| `macaddr`, `macaddr8` | text form | same | `VARCHAR` |
| arrays (any element type) | `to_jsonb(_)::text` → JSON array | `'{1,2,3}'` | `JSON` |
| range / multirange (12 types) | `to_jsonb(_)::text` → JSON | native | `JSON` |
| `tsvector`, `tsquery` | `(_)::text` (PG textual form) | same | `TEXT` (no MySQL equivalent) |
| `point`, `line`, `lseg`, `box`, `path`, `polygon`, `circle` | `(_)::text` (PG textual form) | same | `VARCHAR` (no GIS conversion) |
| `hstore` (extension) | `hstore_to_jsonb(_)::text` → JSON | `'"k"=>"v"'` (PG native) | `JSON` |
| pgvector `vector` | `'[1,2,3]'` (text) | same | TiDB `VECTOR(N)` accepts this directly |
| Composite types | `(_)::text` → PG tuple form `(a,b,c)` (string) | same | `VARCHAR` (use `to_jsonb` view if you want JSON) |
| Enums, domains | `(_)::text` | same | `VARCHAR` |

**Generated columns** (`STORED` / `ALWAYS GENERATED`) are dropped from
the projection so the resulting INSERT is replay-safe.

DDL is **not converted** — `<table>-schema.sql` always contains the
PostgreSQL native definition produced by `pg_dump`. Translate it once
to MySQL/TiDB DDL using the recommended column mapping above (LLMs are
well-suited; the type widths are the only judgement call).

CLI options
-----------

### Connection (PostgreSQL-specific)

```
-h --host           PostgreSQL host (default 127.0.0.1)
-P --port           PostgreSQL port (default 5432)
-u --user           PostgreSQL user (default postgres)
-p --password       password
-B --database       database name (PGDATABASE for the connection)
   --ca / --cert / --key
                    TLS material; --sslmode also accepted as a query param
                    on the DSN (disable / require / verify-ca / verify-full)
```

### Output dialect (PostgreSQL-specific)

```
   --target {mysql,tidb,pg}     SQL output flavor (default: mysql)
                                  mysql: backtick idents, MySQL preamble, X'…' bytea
                                  tidb:  same as mysql, but --no-preamble defaults ON
                                         so files are valid TiDB Lightning input
                                  pg:    PG-native ("…" idents, '\x…' bytea, PG preamble)
   --no-preamble                Suppress the SET-block at the top of SQL files.
                                Required for TiDB Lightning. Defaults ON for tidb,
                                OFF otherwise. Use --no-preamble=false to override.
   --filetype {sql,csv}         Output format (default: sql)
   --csv-output-dialect         snowflake | redshift | bigquery (CSV cell quirks)
   --csv-null-value             default \N
   --csv-separator              default ,
   --csv-delimiter              default "
```

### CDC bootstrap (PostgreSQL-specific, AWS DMS / pg_recvlogical)

```
   --cdc-slot <name>            Atomically create a logical replication
                                slot via CREATE_REPLICATION_SLOT … LOGICAL <plugin>
                                EXPORT_SNAPSHOT, tying it to the dump's MVCC
                                snapshot. Slot name + consistent_point LSN
                                are written to the metadata file
   --cdc-plugin {pgoutput,test_decoding,pglogical_output}
                                Output plugin (default: pgoutput)
   --cdc-cleanup-on-failure     Drop the slot if the dump fails
                                (default: true; set to false to retain the slot)
```

Requires `wal_level = logical` on the source (RDS PG: `rds.logical_replication = 1`).
Slot/plugin names are restricted to `[a-z0-9_]{1,63}` because the
replication protocol does not accept quoted identifiers.

### Consistency

```
   --consistency {auto,snapshot,none}    default: auto (= snapshot)
   --snapshot <token>                    reuse an externally-exported
                                         pg_export_snapshot() token
```

### Throughput

```
-t --threads        Worker count (default 4)
   --rows N         Split tables into N-row chunks (numeric PK range, then
                    ctid range). Without --rows, large tables are dumped
                    by a single worker
   --filesize N     Roll output files at N bytes (KB/MB/GB suffixes accepted)
   --statement-size N   Roll INSERT statements at N bytes (default 1 MB)
   --compress {gzip,snappy,zstd,no-compression}
```

### Filtering

```
-f --filter <glob>  Table filter; e.g. "public.*" or "!pg_catalog.*"
   --no-schemas     Skip the -schema.sql files
   --no-data        Schema only
   --no-views       Skip views (default true)
   --no-sequences   Skip sequences (default true)
```

### Storage backends

`-o`/`--output` accepts a URL: `s3://bucket/prefix?region=…`,
`gs://bucket/prefix`, `azure://container/prefix`, or a local path. All
the upstream `--s3.*`, `--gcs.*`, `--azblob.*` flags are inherited.

### Removed from upstream `dumpling`

`--allow-cleartext-passwords`, `--tidb-mem-quota-query`, the
charset/collation flags, MySQL `flush` / `lock` consistency modes, and
TiKV-specific tuning are all dropped.

Throughput tips
---------------

`--threads` only helps when there are multiple tables OR `--rows` is
set: without `--rows`, each table is dumped by a single worker. For a
small number of large tables:

```sh
./bin/pg-dumpling \
    -h pg -B demo -o /tmp/dump \
    --threads 8 --rows 200000 \
    --filetype csv
```

CSV is typically faster than SQL (no per-row escaping), and the same
output streams cleanly into TiDB Lightning. Chunking uses numeric PK
ranges when available and falls back to `ctid` ranges otherwise; row
count is estimated from `pg_class.reltuples`, so run `ANALYZE` on
freshly-loaded tables before dumping if accurate chunking matters.

Testing
-------

```sh
make test-unit              # short unit tests
make test-unit-failpoint    # full unit tests with failpoint rewriting
                            # (needs `go install github.com/pingcap/failpoint/failpoint-ctl`)
```

CI runs:

- `go vet`, `make build`, unit tests
- end-to-end PG → MySQL SQL roundtrip (`postgres:17` → `mysql:8.4`)
- end-to-end PG → CSV via `LOAD DATA LOCAL INFILE`
- CDC slot bootstrap with LSN + cleanup-on-failure verification
- on tag push, cross-compiled releases for `linux/{amd64,arm64}` and
  `darwin/arm64`

See `.github/workflows/ci.yml`.

Repository layout
-----------------

```
cli/             version metadata
cmd/pg-dumpling/ CLI entry point
context/         logger-aware context
export/          dump engine (PostgreSQL backend)
internal/        vendored helpers (table-filter)
log/             zap wrapper
worklog/         per-step engineering notes
```

Credits
-------

`pg-dumpling` is a substantial rewrite of
[`pingcap/tidb/dumpling`](https://github.com/pingcap/tidb/tree/master/dumpling).
The orchestration design (worker pool, file rolling, IR pipeline,
external storage abstraction, Prometheus metrics) and a great deal of
the test scaffolding all come from the upstream project. Thanks to
PingCAP and the dumpling contributors — without their work this fork
would not exist.

Cloud backends are still backed by `github.com/pingcap/tidb/br/pkg/storage`
(pinned to a public commit of v8.5.6); see `worklog/05-phase3a-public-commit-pin.md`.

License
-------

Apache 2.0, inherited from upstream `pingcap/tidb`. See `LICENSE`.
