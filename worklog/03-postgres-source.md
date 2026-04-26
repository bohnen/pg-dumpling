# 03 — PostgreSQL ソース実装(進行中)

- 日付: 2026-04-26
- 担当: tadatoshi.sekiguchi@pingcap.com (with Claude)
- 状態: build clean。catalog 書き換えは残り多数(03c 以降で継続)

## ゴール

`worklog/01-postgres-rewrite-plan.md` の Step 03。`go-sql-driver/mysql` を `pgx/v5/stdlib` に置換、catalog クエリを PG 用に書き直し、出力方言・型 map・`pg_dump` shell-out 連携を入れて、PG ソース → dumpling 形式の出力ができるようにする。

## 03a — driver 入れ替え + DSN 構築(commit `60db0ca`)

- `config.go`
  - `(conf *Config) GetDriverConfig() *mysql.Config` → `GetDSN(db string) string` に書き直し
  - DSN 形式: `postgres://user:pass@host:port/db?sslmode=...&statement_timeout=...`
  - `Security.SSLMode` フィールド追加(`prefer` をデフォルト)
  - `Security.{CAPath,CertPath,KeyPath}` を `sslrootcert`/`sslcert`/`sslkey` に翻訳
  - `--port` のデフォルトを 4000 → **5432**
  - 不要 import 削除: `go-sql-driver/mysql`、`failpoint`
- `dump.go`
  - `openSQLDB`: `mysql.NewConnector` 系を捨てて `sql.Open("pgx", conf.GetDSN(""))` 一行に
  - `setSessionParam` も DSN 文字列を渡すように
  - `mysql.RegisterDialContext` の `IOTotalBytes` フック削除(failpoint テスト専用、pgx には合わない)
  - `openDB` ヘルパ削除(未使用)
- `sql.go`
  - `ShowCreateDatabase`: `SHOW CREATE DATABASE` クエリを撤廃、`CREATE SCHEMA IF NOT EXISTS "name"` を埋め込み返却
  - `resetDBWithSessionParams`: `*mysql.Config` 引数を DSN 文字列に。`SET SESSION %s = $1`(プレースホルダ表記を MySQL `?` から PG `$1` に)
  - 「未知のシステム変数」ガードを `unrecognized configuration parameter`(PG)と `Unknown system variable`(MySQL 互換)両方マッチに拡張
  - `pgQuoteIdent` / `pgQuoteQName` ヘルパ新設
- `go.mod`: `github.com/jackc/pgx/v5 v5.9.2`、関連の pgpassfile/pgservicefile/puddle を追加。`go-sql-driver/mysql` 削除

## 03b — catalog 探索の最初の一歩(commit `4e366fb`)

- `sql.go`
  - `ShowDatabases`: `SELECT nspname FROM pg_catalog.pg_namespace ...`(`pg_catalog`/`information_schema`/`pg_toast`/`pg_temp_*` を除外)
  - `ShowTables`: `pg_catalog.pg_tables` から `format('%I.%I', ...)` で完全修飾を返す
  - `ShowCreateTable`: `pg_dump --schema-only --no-owner --no-privileges -t schema.table` の child process 起動に置換(`runPgDumpSchema` helper、`SetPgDumpEnv` グローバル経由で接続情報を渡す設計)
  - `buildSelectField`: 列名を `pgQuoteIdent` で `"col"` 形式に
- `writer_util.go`
  - INSERT prefix を `INSERT INTO "schema"."table" VALUES` 形式に(`pgQuoteQName` 経由)
  - `wrapBackTicks` ヘルパ削除
- `ir.go`
  - `newRowsTableMeta` の列名一覧を `pgQuoteIdent` 経由で

## 残作業(03c 以降)

| 領域 | 必要な書き換え |
|---|---|
| `sql.go` `updateSpecifiedTablesMeta` | `SHOW FULL TABLES` / `SHOW TABLE STATUS` を `pg_tables` + `pg_class.reltuples` に |
| `sql.go` `ListAllDatabasesTables` | 同上 + `pg_views` でビュー判定、`information_schema.columns` で `is_generated` |
| `sql.go` `ShowCreateView` | PG では `pg_views.definition` か `pg_dump` で代替 |
| `sql.go` `GetColumnTypes` | `SELECT %s FROM "schema"."table" LIMIT 1`(pgx は型情報を返す) |
| `sql.go` `GetPrimaryKeyColumns` | `pg_index` JOIN `pg_attribute` で PK 取得 |
| `sql.go` `getNumericIndex` | PG 用に書き直し or 削除(数値 PK レンジ chunk は Phase 2 で復活) |
| `sql.go` `GetPartitionNames` | PG パーティションは `pg_partitioned_table` / `pg_inherits` |
| `sql.go` `escapeString` | バックティック escape は不要、関数自体削除へ |
| `sql_type.go` | MySQL 型 map → PG 型 map(`int2/4/8`、`text`、`bytea`、`jsonb` 等)。`escapeBackslashSQL` 削除、`x'...'` → `'\x...'` |
| `writer_util.go` | プリアンブル(`SET standard_conforming_strings`、`SET client_encoding`)を PG 用に |
| `consistency.go` の `SnapshotName` | worker 接続が `SET TRANSACTION SNAPSHOT` を発行する仕組みを wireup |
| `dump.go` の `getSpecialComments` 経由の preamble | PG 用ヘッダに |
| `cmd/dumpling/` → `cmd/pg-dumpling/` | rename と Makefile 連動 |
| `pg_dump` env 配布 | `SetPgDumpEnv` を NewDumper から呼ぶ |
| `retry.go` | `lockTablesBackoffer` 削除 |
| `config.go` 残フラグ | `--allow-cleartext-passwords`、`--tidb-mem-quota-query`、charset/collation を削除 |
| Round-trip 検証 | Docker PG → dump → 別 DB へロード |

## 現在の状態

```sh
$ go build ./... && go vet ./...    # クリーン
$ go build -o bin/pg-dumpling ./cmd/dumpling
$ bin/pg-dumpling --version
Release version: Unknown
Git commit hash: Unknown
...
```

binary は起動するが、まだ `pg_tables` 周辺以外のクエリが MySQL 文法のままのため、PG に対して実行すると catalog 取得段階で失敗する。03c で残りを潰す。
