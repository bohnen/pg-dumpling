# 01 — Postgres 専用化のリライト計画

- 日付: 2026-04-26
- 担当: tadatoshi.sekiguchi@pingcap.com (with Claude)
- 状態: 計画確定、未着手

## ゴール

`pg-dumpling` を **PostgreSQL 専用のダンプツール**として動かす。MySQL/TiDB ソース対応は捨てる(本家 `pingcap/tidb/dumpling` が引き続き存在するため)。データ出力は dumpling 既存のディレクトリ構造・並列モデル・writer パイプライン・外部ストレージ・メトリクスを **そのまま活かす**。DDL は PG ネイティブのまま(必要なら LLM 等で別途変換)。

## 確定事項(2026-04-26 ユーザー承認)

| 項目 | 決定 |
|---|---|
| バイナリ名 | `bin/pg-dumpling`(`bin/dumpling` から改名) |
| DDL 出力方式 | `<table>-schema.sql` は `pg_dump --schema-only -t schema.table` を child process で起動して出力をそのまま書き込む |
| 整合性モード | `--consistency {auto, snapshot, none}` の 3 つのみ。`flush`、`lock` は廃止 |
| スナップショット指定 | `--snapshot <token>` は維持しつつ、意味を「`pg_export_snapshot()` の返す token 文字列」に再定義 |
| 抽象化 | `Source` / `Dialect` interface は **作らない**。PG 直書き |
| 起動フラグ | `--source-type` は **作らない**(単一 backend) |

## 設計方針

dumpling のオーケストレーション(以下)はすべて温存:

- `--threads` worker pool によるテーブル fan-out
- writer pipeline(SQL モード / CSV モード)
- chunked file 出力、`--filesize` でのファイル分割
- `--compress {gzip,snappy,zstd,no-compression}`
- 外部ストレージ(S3/GCS/Azure/local)経由出力
- Prometheus メトリクス、HTTP status エンドポイント
- `br/pkg/storage` / `br/pkg/utils` / `pkg/util/table-filter` / `pkg/util/promutil` への依存

差し替えるのは「データを取りに行く層」と「SQL を吐く層」のみ。

## 削除するコード

### TiDB 内部 import の整理(残るのは 5 つ)

| 依存 | 状態 |
|---|---|
| `pkg/parser`, `pkg/parser/{ast,format,model}` | **削除**(DDL は `pg_dump` 出力をそのまま流すので AST 解析不要) |
| `pkg/tablecodec`, `pkg/store/helper` | **削除**(TiKV region 並列化を捨てる) |
| `pkg/meta/model` | **削除** |
| `pkg/errno` | **削除**(driver のエラーで十分) |
| `pkg/config`, `pkg/infoschema/context` | **削除** |
| `br/pkg/version` | **削除**(サーバ種別判定不要) |
| `pkg/util/codec`, `pkg/util/dbutil`, `pkg/util/filter` | **削除** |
| `pkg/util/table-filter` | **維持** |
| `pkg/util/promutil` | **維持** |
| `pkg/util` | **維持**(`SliceKeysQuoted` 等の数行) |
| `br/pkg/storage` | **維持** |
| `br/pkg/utils` | retry 系のみ維持 |
| `br/pkg/summary` | **維持** |

→ Phase 2 で残った 5 パッケージを `pg-dumpling/internal/` に直接コピーすれば、`go.mod` の `replace github.com/pingcap/tidb => /Users/bohnen/Work/tidb` を完全に外せる。

### dumpling 内部の削除対象

- `export/dump.go`
  - TiKV region 経由 chunk(`concurrentDumpTiDBRegionsTable`、`extractTiDBRowIDFromDecodedKey`)
  - TiDB 専用接続パス、placement policy 出力、sequence 出力(PG では `CREATE SEQUENCE` を pg_dump に任せる)
  - 該当行範囲の目安: L1061-1115、L1592-1670
- `export/sql.go`(56 K → 推定 15 K まで縮む)
  - `SHOW CREATE PLACEMENT POLICY`、`SHOW CREATE SEQUENCE`、`tidb_config` 系、`mysql.stats_histograms`、`TIKV_REGION_STATUS`、`tidb_servers_info`、`SHOW MASTER STATUS`、`SHOW BINARY LOG STATUS`、`SHOW CHARACTER SET`
- `export/consistency.go`
  - `ConsistencyTypeFlush`、`ConsistencyTypeLock` を enum ごと削除
  - `ConsistencyFlushTableWithReadLock`、`ConsistencyLockDumpingTables` 構造体ごと削除
  - 残り: `ConsistencyNone`、`ConsistencySnapshot`(PG 用に書き換え)
- `export/sql_type.go`
  - `escapeBackslashSQL`、`escapeBackslashCSV`(MySQL 流バックスラッシュエスケープ)を削除
  - `SQLTypeBytes` の `x'...'` 出力を `'\x...'` に変更
- `export/writer_util.go`
  - `wrapBackTicks` を `wrapDoubleQuotes` に置換(関数名も)
- `export/config.go`
  - MySQL 専用フラグ(`--allow-cleartext-passwords`、charset/collation 系)を削除
  - TiDB 専用フラグ(`--tidb-mem-quota-query` 等、placement-policy 系)を削除
  - 残すフラグ: `--threads`、`--output`、`--filter`、`--rows`、`--filesize`、`--consistency`、`--snapshot`、`--where`、`--no-data`、`--no-schemas`、`--no-views`、`--csv-*`、`--compress`、S3/GCS/Azure 系、`--host`、`--port`、`--user`、`--password`、`--database`、TLS 系
- `export/metadata.go`
  - GTID/binlog 位置取得を削除(必要なら `pg_current_wal_lsn()` で簡易代替、もしくは metadata file から該当フィールドを落とす)

## 書き換えるコード

### 1. 接続(`config.go`、`dump.go:1378`、`dump.go:1599`、`sql.go:1015`)

```go
import _ "github.com/jackc/pgx/v5/stdlib"

// DSN: postgres://user:pass@host:port/db?sslmode=verify-full&...
db, err := sql.Open("pgx", dsn)
```

`go-sql-driver/mysql` import を捨て、`pgx/v5/stdlib`(`database/sql` ドライバ登録のみ使う)に置換。`*sql.DB` を中心とした既存パイプラインはそのまま流用できる。collation ハードコード(`config.go:270`)も削除し、`client_encoding=UTF8` を SQL ファイルのプリアンブルに移す。

### 2. カタログ探索(`sql.go`)

| 用途 | 採用クエリ |
|---|---|
| schema list | `SELECT nspname FROM pg_namespace WHERE nspname NOT IN ('pg_catalog','information_schema','pg_toast') AND nspname NOT LIKE 'pg_temp_%' AND nspname NOT LIKE 'pg_toast_temp_%'` |
| table list | `SELECT schemaname, tablename FROM pg_tables WHERE schemaname = $1` |
| view list | `SELECT schemaname, viewname FROM pg_views WHERE schemaname = $1`(`--no-views` で抑制可) |
| カラム + 型 | `SELECT column_name, data_type, udt_name, is_generated FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position` |
| PK/index | `pg_index` JOIN `pg_class` JOIN `pg_attribute` で構成。PK は `i.indisprimary` |
| DDL(table-schema.sql) | child process: `pg_dump --schema-only --no-owner --no-privileges -t "<schema>.<table>" -h <host> -p <port> -U <user> -d <db>` の出力をそのまま書き込む |
| 行サイズ概算 | `SELECT pg_relation_size(c.oid) / NULLIF(c.reltuples, 0) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2` |

### 3. 一貫性スナップショット(`consistency.go`、`sql.go`)

メイン接続:

```sql
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT pg_export_snapshot();   -- 取得した token を全 worker に配布
```

各 worker 接続(`--threads` 個):

```sql
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET TRANSACTION SNAPSHOT '<token>';
```

これは `pg_dump -j` と同じ手法。`--threads` の体験を素直に保てる。`--snapshot <token>` を外部から指定された場合は **export を skip** して `SET TRANSACTION SNAPSHOT` だけ使う(再開や外部協調用)。

### 4. 出力方言(`writer_util.go`、`sql_type.go`)

- 識別子: `"name"`(`pgx.Identifier{...}.Sanitize()` ヘルパを使う)
- 文字列: SQL 標準クォート二重化のみ(`'O''Brien'`)
- bytea: `'\x48656c6c6f'`
- プリアンブル(SQL モード):
  ```sql
  SET standard_conforming_strings = on;
  SET client_encoding = 'UTF8';
  SET search_path = pg_catalog;
  ```
- INSERT 文: `INSERT INTO "schema"."table" (cols) VALUES (...)`

### 5. 型マッピング(`sql_type.go:26`)

PG 型 → 既存 3 受信器(`String`/`Number`/`Bytes`)の表に置き換え:

| PG 型 | 受信器 |
|---|---|
| `int2`, `int4`, `int8`, `float4`, `float8`, `numeric`, `oid` | `Number` |
| `bytea` | `Bytes` |
| `text`, `varchar`, `char`, `name`, `bpchar` | `String`(プレーン) |
| `date`, `timestamp`, `timestamptz`, `time`, `timetz`, `interval` | `String` |
| `uuid`, `inet`, `cidr`, `macaddr`, `macaddr8` | `String` |
| `json`, `jsonb` | `String`(必要に応じ `'...'::jsonb` キャスト付与) |
| 配列(`_int4` 等)、レンジ、enum、その他 | `String`(`pgx` が `text` 表現にしてくれる) |

### 6. チャンキング(`dump.go`)

Phase 1: **テーブル単位並列のみ**。`--threads` 個の worker がテーブルキューから取り合う。

Phase 2 候補(必要になったら):

- 数値 PK レンジ: 既存の `getMinMax`/`splitTableWithIndex` を `"col"` クォートに調整するだけで動く
- `ctid` レンジ: pg_dump の `-j` と同じ方式

## 進め方: 2 ステップ

### Step 02 — strip-mysql-tidb(別 commit、`worklog/02-strip-mysql-tidb.md`)

「削除するコード」を実施。

- 上記の TiDB 専用パスを削除
- MySQL 専用パスを削除
- 不要な import を片付ける
- 既存テストの中で MySQL/TiDB 専用ロジックを叩いているものは削除 or skip
- **build は通す**(`go build ./... && go vet ./...`)
- この時点で `bin/dumpling` を起動しても接続段階で MySQL ドライバが居ないので失敗するのは想定内
- コミット → タグ `v0.2.0-stripped` を打って次へ

### Step 03 — postgres-source(別 commit、`worklog/03-postgres-source.md`)

「書き換えるコード」を実施。

- `go-sql-driver/mysql` を捨て `github.com/jackc/pgx/v5/stdlib` を `go.mod` に追加
- `config.go` の DSN を PG 形式に
- `sql.go` のクエリを PG 用に書き直し
- `pg_dump --schema-only` shell-out 実装
- `consistency.go` を `pg_export_snapshot` ベースに
- `sql_type.go` の型 map と blob 表現を PG 用に
- `writer_util.go` の識別子クォートと preamble を PG 用に
- Makefile / cmd 名を `pg-dumpling` に改名(`cmd/dumpling/` → `cmd/pg-dumpling/`、`bin/dumpling` → `bin/pg-dumpling`)
- README / CLAUDE.md 更新
- コミット → タグ `v0.3.0-postgres` を打つ

## 検証

Step 03 完了時点で:

```sh
# 起動可能か
make build
./bin/pg-dumpling --version
./bin/pg-dumpling --help

# Docker で PG を立てる
docker run --rm -d --name pg-test -e POSTGRES_PASSWORD=secret -p 5433:5432 postgres:17
# 適当な DB を作って何件か入れる
psql -h localhost -p 5433 -U postgres -c "CREATE DATABASE demo"
psql -h localhost -p 5433 -U postgres -d demo -c "CREATE TABLE t(id int primary key, s text); INSERT INTO t SELECT g, 'row '||g FROM generate_series(1,1000) g"

# dump
./bin/pg-dumpling -h localhost -P 5433 -u postgres -p secret -B demo -o /tmp/dump --threads 2

# 検証
ls /tmp/dump
cat /tmp/dump/demo.t-schema.sql       # pg_dump --schema-only の出力が入っている
cat /tmp/dump/demo.t.0000000010000.sql # INSERT INTO "demo"."t" VALUES (...)
psql -h localhost -p 5433 -U postgres -c "CREATE DATABASE demo2" && \
  psql -h localhost -p 5433 -U postgres -d demo2 -f /tmp/dump/demo.t-schema.sql && \
  psql -h localhost -p 5433 -U postgres -d demo2 -f /tmp/dump/demo.t.0000000010000.sql
psql -h localhost -p 5433 -U postgres -d demo2 -c "SELECT count(*) FROM t"  # 1000
```

並列 + スナップショット一貫性の検証は別途 `tests/` に sh スクリプトを用意。

## Phase 2 以降の候補

1. テーブル内 chunk(数値 PK レンジ、ctid レンジ)
2. `replace github.com/pingcap/tidb => /Users/bohnen/Work/tidb` の解消(残った 5 パッケージを `internal/` に取り込む)
3. CI(GitHub Actions): build + 単体テスト + PG 起動した integration smoke
4. 大きい型(`hstore`、`PostGIS geometry`、`tsvector`、ユーザ定義 enum)の動作確認とドキュメント
