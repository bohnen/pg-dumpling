# pg-dumpling — Claude 向けプロジェクトメモ

## このプロジェクトは何か

`pg-dumpling` は **PostgreSQL 専用**のダンプツール。出力フォーマットは upstream
dumpling と同じディレクトリ構造を踏襲し、最終的な狙いは **PostgreSQL → MySQL /
TiDB のデータ移行ツール**として使えるようにすること。

- DDL は PG ネイティブ(`pg_dump --schema-only` の出力をそのまま埋め込む)。
  TiDB 用に変換する必要があれば LLM 等で別途行う。
- データは SQL 標準のリテラル形式で出力するため、適切な preamble と移行先 schema
  があれば MySQL family にロードできる**見込み**。Phase 2 でこれを実証する。
- MySQL/TiDB ソース対応はバッサリ削除済み(本家 dumpling が引き続きカバー)。

## マイルストーン

| タグ | 状態 | 内容 |
|---|---|---|
| `v0.1.0-baseline` | ✅ | TiDB monorepo から切り出した素の dumpling |
| `v0.2.0-strip-consistency` | ✅ | consistency lock/flush 削除、テスト一掃 |
| `v0.3.0-postgres` | ✅ | PG 用 catalog / driver / dialect、bin/pg-dumpling リネーム、Docker round-trip 成功 |
| `v0.4.0` | ✅ | Phase 2 完了: CSV server-side cast 21 型、テーブル内 chunk(数値 PK + ctid)、TiDB 内部依存 7→1、TiDB v8.5.6 への CSV 移行検証成功 |
| `v0.5.0` | ✅ | Phase 3a: `replace ~/Work/tidb` 撤去。`github.com/pingcap/tidb` を public commit pin(`v1.1.0-beta.0.20260413061245-ae18096e0237` = v8.5.6)に切替 |
| `v0.6.0` | ✅ | Phase 3: SQL 出力を MySQL/TiDB 向けに修正。`--target {mysql,tidb,pg}` を追加、デフォルト `mysql`。MySQL/TiDB 直叩きルート(`mysql -h … < dump.sql`)が確立 |
| `v0.7.0` | ✅ | Phase 4a: PG 追加型 11 種(bit/varbit、geometric 7 種、hstore、composite フォールスルー)。総カバー 35+ 型 |
| `v0.8.0` | ✅ | Phase 4b: CDC ブートストラップ。`--cdc-slot/--cdc-plugin/--cdc-cleanup-on-failure` で論理レプリケーションスロットを atomic 作成、metadata に LSN 記録、AWS DMS 連携可能 |

## ビルド & テスト

```sh
make build                  # bin/pg-dumpling
make test-unit              # 単体(限定的、PG 用は Phase 2 で拡充)
make test-unit-failpoint    # failpoint 注入版
make tidy                   # go mod tidy
make vet                    # go vet ./...
make clean                  # bin/ 削除
```

`bin/pg-dumpling --version` / `--help` が動けば最低限の健全性 OK。

## 重要な制約

### TiDB 依存は public commit pin に解決済み(v0.5.0)

`go.mod` は `github.com/pingcap/tidb v1.1.0-beta.0.20260413061245-ae18096e0237`
(= v8.5.6 commit `ae18096e0237`)に固定。**ローカルチェックアウトは不要**で、
`go build` は `proxy.golang.org` から透明に解決する。

実際に import している TiDB 内部パッケージは `br/pkg/storage` 1 個だけ
(S3/GCS/Azure/ローカルの統一抽象)。これはクラウドバックエンド対応の
中核なので意図的に残している。

依存を更新したくなったら:

```sh
go get github.com/pingcap/tidb@<commit-or-tag>
go mod tidy
```

`replace` ディレクティブには TiDB 上流 go.mod から継承した非 TiDB 系のもの
(`apache/arrow-go`、`go-ldap`、`sourcegraph` 系)だけが残っている。

### `pg_dump` を child process として呼ぶ

`<table>-schema.sql` は `runPgDumpSchema` が `pg_dump --schema-only` を起動して
標準出力をキャプチャする。`PATH` に `pg_dump` が必要。接続情報は
`setupPgDumpEnv` step で `PGHOST` / `PGPORT` / `PGUSER` / `PGPASSWORD` /
`PGSSLMODE` 等の env として渡す。

### snapshot 一貫性の流れ

`ConsistencyTypeAuto` → `Snapshot` に解決され、`ConsistencySnapshot.Setup` が:

1. メイン接続で `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`
2. `SELECT pg_export_snapshot()` で token 取得
3. `SetPgSnapshotToken(token)` でグローバルに公開
4. 各 worker は `createConnWithConsistency` 経由で `BEGIN ...` + `SET
   TRANSACTION SNAPSHOT '<token>'` を実行

`--snapshot <token>` で外部 token を持ち込むこともできる(`Setup` は export を
skip して `SET` だけ流す)。

### 出力 SQL の方言

`--target` フラグで切替(デフォルト `mysql`):

| | `--target=mysql` / `tidb`(デフォルト) | `--target=pg` |
|---|---|---|
| 識別子 | `` `schema`.`table` ``、`` `col` `` | `"schema"."table"`、`"col"` |
| 文字列 | `'O''Brien'`(`NO_BACKSLASH_ESCAPES` 有効化で SQL 標準) | `'O''Brien'` |
| bytea | `X'48656C6C6F'`(MySQL hex literal) | `'\x48656c6c6f'`(PG hex literal) |
| Preamble | `SET NAMES utf8mb4; FOREIGN_KEY_CHECKS=0; UNIQUE_CHECKS=0; sql_mode='NO_BACKSLASH_ESCAPES'` | `SET standard_conforming_strings = on; client_encoding = 'UTF8'; search_path = pg_catalog;` |
| timestamptz / interval / array / inet / json | server-side cast(`pgMigrationCast`)で MySQL 互換テキスト化 | PG ネイティブ表現のまま |
| `<schema>-schema-create.sql` | `` CREATE DATABASE IF NOT EXISTS `name` `` | `CREATE SCHEMA IF NOT EXISTS "name"` |

CSV モードは `--target` の影響を受けない(常に MySQL/TiDB 互換)。

`<table>-schema.sql` の DDL は引き続き **PG ネイティブ**(`pg_dump --schema-only`
の出力)。MySQL/TiDB に取り込むときはユーザ側で別途変換すること(LLM 等で)。

### Postgres と MySQL の型ギャップ

PG → MySQL/TiDB の SQL リテラル互換は範囲が広く、SQL レベルで埋めるのは現実的
ではない(Phase 1 末の検証で Lightning 系ではなく素の MySQL に流したところ、
preamble 段階で即時失敗)。

Phase 2 の方針: **CSV を経由**し、PG 型ごとに `to_jsonb(col)::text` /
`to_char(col AT TIME ZONE 'UTC', ...)` / `encode(col, 'hex')` 等のサーバ側 cast
を SELECT 句に仕込んで安全な文字列化を行う。詳細マトリクスは
`worklog/04-phase2-roadmap.md` 参照。

**bytea のロード(重要なレシピ)**: `LOAD DATA INFILE` は CSV セルから直接
BLOB / VARBINARY に流せない(改行・引用符・NULL バイトが境界を壊す)。
pg-dumpling は CSV に hex 文字列を書くので、ローダ側で
`(@col_hex) SET col = UNHEX(@col_hex)` で復元する。NULL は `\N` のまま素通り。
詳細は `worklog/04-phase2-roadmap.md` の「ローダ側の TIPS」。

## ディレクトリ構成

```
cli/             バージョン情報
cmd/pg-dumpling/ main(import 5 行)
context/         logger 付き context
export/          dump engine(PG 専用に書き換え済み)
log/             zap ラッパ
worklog/         作業ログ(00→01→02→03→04)
```

`export/` の主要ファイル:

- `config.go`: PG DSN 構築、フラグ定義
- `consistency.go`: `pg_export_snapshot()` ベースの一貫性
- `conn.go`: retry を持つ DB 接続ラッパ(汎用)
- `dump.go`: オーケストレーション(worker pool、fan-out)
- `sql.go`: PG 用カタログクエリ、`pg_dump` shell-out
- `sql_type.go`: PG 型 → 受信器の map、SQL 標準エスケープ
- `writer.go`、`writer_util.go`: ファイル出力
- `ir.go`、`ir_impl.go`: 行→文字列の中間表現
- `metadata.go`: 現状 no-op
- `prepare.go`、`block_allow_list.go`: テーブル列挙とフィルタ

## 削除済の MySQL/TiDB 残骸

Phase 1 末で完全削除:

- `--allow-cleartext-passwords`、`--tidb-mem-quota-query`
- `Config` 構造体の `AllowCleartextPasswords`、`TiDBMemQuotaQuery`、
  `CollationCompatible`、`Net`、`IOTotalBytes` フィールド
- `LooseCollationCompatible` / `StrictCollationCompatible` 定数
- `lockTablesBackoffer`、`getTableFromMySQLError`(retry.go)
- `failpoint("SetIOTotalBytes")` フック

## Phase 2 完了内容(v0.4.0)

- **CSV server-side cast**(`pgMigrationCast` in `sql.go`): 21 種の PG 型
  (timestamptz / timestamp / time(tz) / date / interval / bool / bytea / uuid /
  json / jsonb / inet / cidr / macaddr / 配列 / range / enum / pgvector / tsvector 等)
  を `--filetype=csv` 時に MySQL/TiDB 互換の文字列に変換。SQL モードは PG ネイティブ literal を維持
- **テーブル内 chunk 復活**: 数値 PK レンジ(`getNumericIndex`)→ `ctid` レンジ
  (`concurrentDumpTableByCtid`)→ 単一 dump のフォールバック。`--rows N` で chunk 行数指定
- **行数推定**: `pg_class.reltuples` ベースに置換(MySQL `EXPLAIN` パーサ撤去)
- **TiDB 内部依存 7 → 1**:
  - 削除: `br/pkg/version`、`pkg/util/promutil`、`br/pkg/utils.WithRetry`
    (inline)、`br/pkg/summary`(zap log 化)、`pkg/util` の TLS(stdlib 化)
  - vendor: `pkg/util/table-filter` を `internal/table-filter/` に
  - 残置: `br/pkg/storage`(クラウドバックエンド一式は dumpling の魅力なので温存)

## Phase 3 完了内容(v0.6.0)

- **`--target {mysql, tidb, pg}` フラグ追加**(デフォルト `mysql`)。SQL 出力の方言を選択可能に
- **`SQLDialect` interface**(`export/dialect.go` 新設):pg / mysql 実装を分離。識別子クォート、preamble、bytea リテラル、CREATE DATABASE/SCHEMA、migration cast の有無を吸収
- **`pgMigrationCast` を SQL モードでも有効化**(`--target=mysql/tidb` のとき)。bytea は SQL モードでは encode せず、SQLTypeBytes が `X'…'` 形式で出力
- **`tableMeta.selectColumns` 新設**: SELECT 式と INSERT カラムリストを分離。INSERT には dialect-quoted のカラム名のみ出る
- **e2e 検証**: `postgres:17` から dump → `mysql:8.4` と `tidb:v8.5.6` で `mysql -h … < dump.sql` 直叩きが成功。21 型 allty テーブル、bytea(NULL バイト含む)、interval、JSON 配列など全て一致

## Phase 3a 完了内容(v0.5.0)

- **`replace ~/Work/tidb` 撤去**: `github.com/pingcap/tidb` を public commit pin
  (`v1.1.0-beta.0.20260413061245-ae18096e0237` = v8.5.6 commit `ae18096e`)
  に切替。`go.mod` の require 行を pseudo-version に更新し、ローカル
  チェックアウト依存を完全に解消
- `pkg/parser` も同様に pseudo-version(`v0.0.0-20260331085336-4e0b702f38a8`)で固定
- `proxy.golang.org` 経由で透明に解決され、初回 `go mod download` 後はキャッシュから取得
- `replace` には TiDB 上流由来の非 TiDB 系(`apache/arrow-go`、`go-ldap`、`sourcegraph` 系)のみが残る

## Phase 4b 完了内容(v0.8.0)

- **CDC ブートストラップ**: `--cdc-slot <name>` で論理レプリケーションスロットを
  ダンプの MVCC スナップショットと **atomic に**作成
  (`CREATE_REPLICATION_SLOT … LOGICAL <plugin> EXPORT_SNAPSHOT`)
- 対応プラグイン: `pgoutput`(default)、`test_decoding`、`pglogical_output`
- `metadata` ファイルに `CDC Slot` / `CDC Plugin` / `CDC Consistent Point (LSN)` /
  `CDC Snapshot Name` を出力
- `--cdc-cleanup-on-failure`(default ON): ダンプ失敗時にスロットを drop し、
  WAL を蓄積する事故を防止
- 識別子バリデーション: replication プロトコルが quoted identifier を受け付け
  ないため、slot/plugin 名は `[a-z0-9_]{1,63}` に制限
- AWS DMS 連携: source endpoint の `pluginName` / `slotName` extra connection
  attributes に metadata の値を渡し、CDC-only タスクとして起動
- **前提**: PG 側で `wal_level=logical`、RDS なら `rds.logical_replication=1`
- 詳細・運用注意: `worklog/08-phase4b-cdc-bootstrap.md`

## Phase 4a 完了内容(v0.7.0)

- **bit / bit varying**: `(col)::text` で文字列リテラル化(`'10101010'`)
- **Geometric 7 種**(`point`/`line`/`lseg`/`box`/`path`/`polygon`/`circle`):
  PG native 形式を文字列化(`'(1,2)'` 等)
- **hstore**: `hstore_to_jsonb(col)::text` で JSON 化(target=mysql/tidb のとき)。
  target=pg では native `"k"=>"v"` 維持
- **composite types**: `::text` フォールスルー(PG タプル形式 `(a,b,c)` を文字列化)
- e2e 検証: `postgres:17` ↔ `mysql:8.4` ↔ PG round-trip で 11 型 + 既存 21 型混在を確認
- 詳細: `worklog/07-phase4a-additional-types.md`

## Phase 4 残候補

- **CI**(GitHub Actions: build + PG smoke + MySQL/TiDB smoke + CDC smoke)
- **PostGIS** `geometry`/`geography`(WKT/WKB の選択、SRID 扱い、外部依存)
- **Parquet 出力**(上流 dumpling にも無いので必要性次第)

MySQL 互換 SQL 出力モード(`--target=mysql/tidb` フラグ)は **採用しない**。

## 作業のお作法

- 作業ログは `worklog/<連番>-<短い説明>.md` に時系列で残す
- import path 一括置換は `find . -name '*.go' -exec sed -i '' '...' {} +`
- bazel `BUILD.bazel` は持たない(`go build` のみ)
- failpoint 関連テストは `make test-unit-failpoint` で `failpoint-ctl enable` してから走らせる
- Docker での round-trip 検証は PG(`postgres:17`)と MySQL(`mysql:8.4`)の両方で行う

## ライセンス

Apache 2.0、上流 `pingcap/tidb` 由来。
