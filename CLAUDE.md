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
| Phase 2(進行中) | 🚧 | **中間フォーマット**経由の PG → TiDB 移行: CSV ハードニング、Parquet 出力、テーブル内 chunk、`replace` 解消 |

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

### TiDB ローカルチェックアウトに依然として依存

`go.mod` の `replace github.com/pingcap/tidb => /Users/bohnen/Work/tidb` を消すと
build できない。残っている TiDB パッケージは:

- `br/pkg/storage`(S3/GCS/Azure 抽象)
- `br/pkg/utils`(retry helper)
- `br/pkg/summary`(進捗サマリ)
- `pkg/util/{table-filter, promutil}`、`pkg/util`(SliceKeysQuoted 等の数行)

Phase 2 でこれらを `internal/` に取り込む方針。

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

- 識別子: `"schema"."table"`、`"col"`(double-quote)
- 文字列: SQL 標準クォート二重化(`'O''Brien'`)
- bytea: PG リテラル `'\x48656c6c6f'`
- 各データファイル(SQL モード)の先頭に preamble:
  ```sql
  SET standard_conforming_strings = on;
  SET client_encoding = 'UTF8';
  SET search_path = pg_catalog;
  ```
- **重要**: この SQL モード出力は PG 専用(同じ PG クラスタへの round-trip
  検証用)。MySQL/TiDB に取り込もうとすると `Unknown system variable
  'standard_conforming_strings'` で弾かれる。
- Phase 2 では SQL 互換出力は追わず、**CSV / Parquet を中間フォーマット**として
  整備し、TiDB Lightning 等のローダ経由で TiDB / MySQL へ流す。

### Postgres と MySQL の型ギャップ

PG → MySQL/TiDB の SQL リテラル互換は範囲が広く、SQL レベルで埋めるのは現実的
ではない(Phase 1 末の検証で Lightning 系ではなく素の MySQL に流したところ、
preamble 段階で即時失敗)。

Phase 2 の方針: **CSV / Parquet を経由**し、PG 型ごとに `to_jsonb(col)::text` /
`to_char(col AT TIME ZONE 'UTC', ...)` / `encode(col, 'hex')` 等のサーバ側 cast
を SELECT 句に仕込んで安全な文字列化を行う。詳細マトリクスは
`worklog/04-phase2-roadmap.md` 参照。

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

## Phase 2 の作業項目

`worklog/04-phase2-roadmap.md` を主参照。要点は:

1. **CSV ハードニング** — PG 固有型(timestamptz / bytea / uuid / json /
   配列 / range / enum / interval / inet 系)をサーバ側 cast で安全な文字列にして
   出力。`--csv-binary-format`、bool の 0/1 化等のオプション追加
2. **Parquet 出力** — `--filetype parquet`。`information_schema.columns` から
   Parquet schema を導出、`apache/arrow-go` を使ってストリーミング書き出し
3. **テーブル内 chunk** — 数値 PK レンジ → `ctid` レンジ → partition fan-out
4. **`replace ~/Work/tidb` の解消** — 残った 5 パッケージを `internal/` 取り込み
5. **ドキュメント整備と `v0.4.0` リリース**

CI(GitHub Actions)、PostGIS / hstore / tsvector の特殊型対応は Phase 3 へ送る。
MySQL 互換 SQL 出力モード(`--target=mysql/tidb` フラグ)は **採用しない**。

## 作業のお作法

- 作業ログは `worklog/<連番>-<短い説明>.md` に時系列で残す
- import path 一括置換は `find . -name '*.go' -exec sed -i '' '...' {} +`
- bazel `BUILD.bazel` は持たない(`go build` のみ)
- failpoint 関連テストは `make test-unit-failpoint` で `failpoint-ctl enable` してから走らせる
- Docker での round-trip 検証は PG(`postgres:17`)と MySQL(`mysql:8.4`)の両方で行う

## ライセンス

Apache 2.0、上流 `pingcap/tidb` 由来。
