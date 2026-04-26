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
| Phase 2(進行中) | 🚧 | PG → MySQL/TiDB 移行: 型変換、MySQL ターゲット dialect、テーブル内 chunk、CI |

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
- 各データファイルの先頭に preamble:
  ```sql
  SET standard_conforming_strings = on;
  SET client_encoding = 'UTF8';
  SET search_path = pg_catalog;
  ```
- **重要**: この preamble は MySQL/TiDB では `Unknown system variable` で弾かれる。
  Phase 2 で `--target {postgres,mysql,tidb}` フラグを足し、ターゲットごとに
  preamble と識別子クォートを切り替える予定。

### Postgres と MySQL の型ギャップ

Phase 1 時点では PG → MySQL/TiDB ロードに以下のギャップがある:

| PG 型 | 出力リテラル | MySQL 取込 |
|---|---|---|
| `int2/4/8`、`numeric` | 数値 | OK |
| `text`、`varchar`、`uuid` | `'...'` | OK(SQL_MODE で `ANSI_QUOTES` 有効化必要) |
| `bytea` | `'\xHEX'` | **NG**(MySQL は `x'HEX'` か `0xHEX`) |
| `jsonb` | `'{"a":1}'` | OK(`JSON` カラムへ) |
| `timestamptz` | `'2026-04-26T21:00:00+09:00'` | **要 cast / 切り捨て**(MySQL は TZ 情報を保持しない) |
| `date`、`time`、`interval` | 文字列 | 形式調整が必要 |
| 配列、レンジ、enum | 文字列 | **NG**(MySQL に直接対応なし、JSON 化 or 別表化) |

これら全てが Phase 2 のスコープ。

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

1. **`--target {postgres,mysql,tidb}` フラグ**で出力 dialect を切り替え
2. **型変換マトリクス**(`bytea` の `'\xHEX'` ↔ `x'HEX'`、`timestamptz` の TZ 切り捨て、配列の JSON 化、enum の文字列展開、`numeric` の桁適合)
3. **テーブル内 chunk**(数値 PK レンジ、`ctid` レンジ)
4. **`pg_dump` 出力の MySQL/TiDB DDL 化レシピ**(LLM 連携を念頭に)
5. **`replace ~/Work/tidb` の解消**(残った 5 パッケージを `internal/` 取り込み)
6. **CI**(GitHub Actions: build + PG smoke + MySQL/TiDB smoke)
7. **PostGIS / hstore / tsvector** 等の特殊型対応

## 作業のお作法

- 作業ログは `worklog/<連番>-<短い説明>.md` に時系列で残す
- import path 一括置換は `find . -name '*.go' -exec sed -i '' '...' {} +`
- bazel `BUILD.bazel` は持たない(`go build` のみ)
- failpoint 関連テストは `make test-unit-failpoint` で `failpoint-ctl enable` してから走らせる
- Docker での round-trip 検証は PG(`postgres:17`)と MySQL(`mysql:8.4`)の両方で行う

## ライセンス

Apache 2.0、上流 `pingcap/tidb` 由来。
