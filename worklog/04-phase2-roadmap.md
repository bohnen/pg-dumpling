# 04 — Phase 2 roadmap: PostgreSQL → MySQL / TiDB データ移行

- 日付: 2026-04-26
- 担当: tadatoshi.sekiguchi@pingcap.com (with Claude)
- 状態: 計画。実装着手前

## ゴール

Phase 1(`v0.3.0-postgres`)で「PG → dumpling 形式 → PG」の round-trip が成立した。
Phase 2 のメインゴールは **PG → MySQL/TiDB のデータ移行ツールとして実用化**する
こと。具体的には:

1. **重要マイルストーン**: PG から取得したデータを TiDB(または MySQL)に
   import できる
2. **テーブル内 chunking** を復活させ、`--threads` 並列性をテーブル数より細かく
   活かせるようにする
3. **`replace ~/Work/tidb` を消す**(完全独立リポジトリ化)
4. **CI** で PG round-trip と PG → TiDB smoke を自動実行

## 動機(Phase 1 末に判明したギャップ)

Phase 1 で Docker `mysql:8.4` に PG ダンプをロードしようとして得た現実:

```
$ mysql -uroot < /tmp/pg2my/public.simple.000000000.sql
ERROR 1193 (HY000) at line 1: Unknown system variable 'standard_conforming_strings'
```

INSERT ブロック先頭の preamble 3 行(`SET standard_conforming_strings = on;` 等)
が MySQL で全部弾かれる。INSERT 自体に到達せず、データはゼロ件入る。
「とりあえずそのまま流す」が成り立たないため、ターゲット dialect を意識した
**マルチターゲット出力**を Phase 2 で実装する必要がある。

これは preamble だけでなく、識別子クォート・リテラル・型表現すべてに波及する。

## 課題 1: マルチターゲット出力 dialect

新フラグ案: `--target {postgres,mysql,tidb}`(default `postgres`)。

| 項目 | `--target=postgres` | `--target=mysql` | `--target=tidb` |
|---|---|---|---|
| Preamble | `SET standard_conforming_strings = on; SET client_encoding = 'UTF8'; SET search_path = pg_catalog;` | `/*!40101 SET NAMES utf8mb4*/; /*!40014 SET FOREIGN_KEY_CHECKS=0*/;` | `/*!40101 SET NAMES utf8mb4*/; /*!40014 SET FOREIGN_KEY_CHECKS=0*/;` + 必要なら TiDB 専用 hint |
| 識別子クォート | `"col"` | `` `col` `` | `` `col` `` |
| 文字列エスケープ | `''` 二重化のみ | バックスラッシュ + `''` 二重化(MySQL 流) | 同 MySQL |
| BYTEA リテラル | `'\xDEADBEEF'` | `x'DEADBEEF'` または `0xDEADBEEF` | 同 MySQL |
| BOOL | `true` / `false` | `1` / `0` | `1` / `0` |
| TIMESTAMPTZ | `'2026-04-26T21:00:00+09:00'` | TZ 情報を切り捨てて `'2026-04-26 12:00:00'`(UTC 化) | 同 MySQL |
| JSONB | `'{"a":1}'` | `'{"a":1}'`(target カラムが JSON 型なら可) | 同 MySQL |
| INTERVAL | `'1 day 02:03:04'` | 文字列 or 秒数(target 仕様に依存) | 同 MySQL |
| 配列・レンジ・enum | 文字列 | JSON 文字列化 or 別表展開 | 同 MySQL |
| ファイル拡張子 / 命名 | 既存どおり | 既存どおり | 既存どおり |

**実装方針**:

- `export/dialect.go` に `type Dialect interface` を切る — まず enum + switch で
  始め、interface 化は分岐が増えてから
- `pgQuoteIdent` を `Dialect.QuoteIdent(name)` に置き換え
- `escapeSQL` / `SQLTypeBytes.WriteToBuffer` 等を Dialect 経由で
- preamble は `getSpecialComments(target)` に
- `MakeRowReceiver` が型に応じて Dialect を考慮した receiver を返す(BOOL の
  `t/f` → `1/0` 変換等)

## 課題 2: PG 固有型のロード可能化

PG → TiDB import で必要になる型変換。`--target=tidb` 時に dumper 側で実施するか、
あるいは TiDB Lightning 等の loader 側で対応するかは選択肢があるが、まず
**dumper 側で SQL リテラルを互換化**する方向で考える(loader を限定したくない)。

| PG 型 | 想定する TiDB 側カラム | dumper の挙動 | 備考 |
|---|---|---|---|
| `int2`、`int4`、`int8` | `SMALLINT`、`INT`、`BIGINT` | そのまま数値 | OK |
| `numeric(p,s)` | `DECIMAL(p,s)` | そのまま数値 | OK |
| `float4`、`float8` | `FLOAT`、`DOUBLE` | そのまま数値 | NaN/Inf は `'NaN'` 文字列 → 要対応 |
| `bool` | `TINYINT(1)` | `t`/`f` を `1`/`0` に | dialect マトリクス |
| `text`、`varchar`、`char`、`uuid` | `TEXT`、`VARCHAR`、`CHAR(36)` | `'...'` (バックスラッシュエスケープ on) | エスケープ規則を target に合わせる |
| `bytea` | `VARBINARY`、`BLOB` | `x'HEX'` | dialect マトリクス |
| `date`、`time` | `DATE`、`TIME` | `'YYYY-MM-DD'` / `'HH:MM:SS'` | 6 桁の小数秒は MySQL も TIME(6) で OK |
| `timestamp` | `DATETIME`(6) | `'YYYY-MM-DD HH:MM:SS.ffffff'` | OK |
| `timestamptz` | `TIMESTAMP`(6) | UTC に正規化して `'YYYY-MM-DD HH:MM:SS'` | TZ を捨てる旨をログに |
| `interval` | `INT`(秒) or `VARCHAR` | `EXTRACT(EPOCH FROM ...)` 換算 or 文字列 | デフォルト動作要決定 |
| `json`、`jsonb` | `JSON` | `'…'` をそのまま | OK(MySQL/TiDB は JSON 型サポート) |
| `inet`、`cidr` | `VARCHAR(43)` | 文字列化 | OK |
| `macaddr`、`macaddr8` | `VARCHAR(17/23)` | 文字列化 | OK |
| `_int4` 等の配列 | `JSON` | `array_to_json(col)::text` で取り出し | Phase 2 中盤 |
| range/multirange(`int4range`、`tstzrange` 等) | `JSON`(`{"l":..,"u":..}`) | server 側 cast or client 側変換 | Phase 2 中盤 |
| ユーザ定義 enum | `VARCHAR` or `ENUM(...)` | 文字列化 | OK |
| `tsvector`、`tsquery` | `TEXT` | 文字列化 | 検索性は失われる、ドキュメント化 |
| `geometry`(PostGIS) | TiDB の地理関数なし | `ST_AsEWKB(col)::bytea` で取り出し → `JSON` 等 | 別ユースケース、後回し |
| `hstore` | `JSON` | `hstore_to_json(col)` を SELECT 側で適用 | 後回し |
| 配列の配列、ドメイン型 | ケースバイケース | ベース型に再帰 | 後回し |

**実装方針**:

- 受信器を 3 種(Number/String/Bytes)から拡張: `SQLTypeBool`、`SQLTypeTimestampTZ`、`SQLTypeInterval`、`SQLTypeArray`、`SQLTypeRange` 等
- 型 map(`colTypeRowReceiverMap`)を `--target` で枝分けせず、受信器内部で
  target を見て分岐(現状の `escapeBackslash bool` パラメータ流儀の拡張)
- 配列 / range / hstore / PostGIS は **SELECT 側で server-side cast** が単純:
  `SELECT col1, array_to_json(arr_col)::text AS arr_col, ...` を `buildSelectQuery`
  に組み込む。`information_schema.columns` の `data_type` を読むだけで分岐可能

## 課題 3: テーブル内 chunking の復活

Phase 1 では `getNumericIndex` が空文字を返し、テーブル内 chunk は無効化。
Phase 2 で復活させる:

1. **数値 PK レンジ chunk**(優先度高、上流 dumpling と互換)
   - `getNumericIndex` を PG 用に再実装: `pg_index` で PK / unique を取得 →
     `pg_attribute` から型確認 → 整数型なら採用
   - 既存の `getMinMax` / `splitTableWithIndex` をそのまま流用(クォート修正のみ)
   - `--rows N` フラグの no-op を解除

2. **`ctid` レンジ chunk**(PG 特有、PK が無いテーブル向け)
   - `pg_relation_size / current_setting('block_size')::int` でブロック数を推定
   - `ctid >= '(START,0)' AND ctid < '(END,0)'` で WHERE 構築
   - `pg_dump --jobs N` と同じ手法

3. **partitioned table の partition 単位 fan-out**(`pg_inherits` 子表)
   - `GetPartitionNames` は既に Phase 1 で実装済み
   - `concurrentDumpTable` から partition ごとに dump task を発行する経路を復活

## 課題 4: `replace ~/Work/tidb` の解消

`go.mod` から `replace` を消すには、現在 import している以下を `internal/` に
取り込むか書き直す必要がある:

| 依存 | 現用途 | 取り込み方針 |
|---|---|---|
| `br/pkg/storage` | S3/GCS/Azure 抽象 | そのままコピー(BSL 互換は問題なし、Apache 2.0) |
| `br/pkg/utils` | retry helper | `WithRetry` 関数 1 個だけ、自前実装で代替 |
| `br/pkg/summary` | progress 集計 | コピー or 簡素化(zap log で十分という選択もあり) |
| `pkg/util/table-filter` | `--filter` 解析 | コピー(独立性の高いパッケージ) |
| `pkg/util/promutil` | metrics ファクトリ | コピー(数行) |
| `pkg/util` | `SliceKeysQuoted` 等 | 必要関数のみ inline |

これで `replace github.com/pingcap/tidb => /Users/bohnen/Work/tidb` を削除し、
**完全独立な Go モジュール**になる。CI もこれを前提にできる。

## 課題 5: CI(GitHub Actions)

`.github/workflows/ci.yml` で:

```yaml
- go build ./...
- go vet ./...
- make test-unit
- spin up postgres:17 service container
  - PG round-trip smoke (1000 行 + 多型テーブル)
- spin up mysql:8.4 service container
  - --target=mysql で dump → mysql -e でロード → 件数照合
```

`replace` 解消後に GitHub に push 可能になるので、その後の作業。

## 課題 6: ドキュメント整備

- `docs/migration-postgres-to-tidb.md`(仮)— ユーザ向け。型マトリクスを公開
- `docs/limitations.md` — PG 固有機能の制約一覧(PostGIS、hstore、tsvector、
  パブリケーション、トリガ、ストアドプロシージャ等は dump 対象外)
- `docs/recipes/` — `pg_dump --schema-only` 出力を TiDB 用に LLM 変換するための
  プロンプト・テンプレ集

## 着手順序(提案)

1. **04a — Dialect 切り替え骨格**: `--target` フラグ + preamble + 識別子クォート + bytea リテラル切り替え。MySQL ターゲットで小さい round-trip を成立させる
2. **04b — 型受信器の target 対応**: bool、timestamptz、interval、配列/range の
   server-side cast 経由出力
3. **04c — テーブル内 chunk 復活**: 数値 PK 優先 → ctid 補完
4. **04d — TiDB 依存解消**: `replace` 削除、`internal/` 取り込み
5. **04e — CI**: PG smoke + TiDB smoke + lint
6. **04f — ドキュメント整備とリリース**: README に migration ガイド、型マトリクス公開、`v0.4.0` タグ

各段は単独 commit / PR にできる粒度を意図している。

## 非ゴール(Phase 2 ではやらない)

- スキーマ DDL の自動 PG → MySQL/TiDB 変換(LLM 任せの方針)
- 完全な PG 機能カバレッジ(PostGIS、hstore、tsvector、ユーザ定義型)— 制約として
  ドキュメント化のみ
- パブリケーション・トリガ・ストアドプロシージャの dump
- 双方向同期(あくまでも一方向の dump)
