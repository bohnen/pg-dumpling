# 04 — Phase 2 roadmap: 中間フォーマットによる PG → TiDB 移行

- 日付: 2026-04-26
- 担当: tadatoshi.sekiguchi@pingcap.com (with Claude)
- 状態: 計画。実装着手前

## 方針(2026-04-26 確定)

Phase 2 は **CSV / Parquet 等の中間フォーマット**を一級市民として整備し、それを
ローダ(TiDB Lightning 等)経由で TiDB / MySQL に取り込む経路を実用化することを
ゴールとする。

直接ロード可能な「MySQL 互換 SQL」を吐く方向は **採用しない**:

- PG ↔ MySQL の非互換は型・関数・SQL_MODE・トランザクション・予約語・大文字小文字・
  TZ 取り扱いに渡って多岐にわたり、`SET …` 一発では揃わない
- 中間フォーマットなら型変換ロジックがシンプル(serializer 1 箇所に集中)で、
  ローダ側のバグ修正・最適化の恩恵を受けられる
- TiDB Lightning は CSV / Parquet をネイティブに受けられる

`--target=postgres` フラグは不要(用途は `pg_dump` でカバー済み)。SQL 出力モードは
小規模 round-trip / デバッグ用として現状を維持するが、移行用途では使わない。

## ゴール

1. **CSV を TiDB Lightning に流せる品質に**(PG 固有型の安全な文字列化、
   `\N` NULL、適切なバイナリ表現)
2. **Parquet 出力を追加**(`--filetype parquet`)、PG 型 → Parquet 論理型の
   素直なマッピング
3. **テーブル内 chunking 復活**(数値 PK レンジ / ctid レンジ / partition 子表)
4. **`replace ~/Work/tidb` の解消**(完全独立モジュール化)
5. ドキュメント整備と `v0.4.0` リリース

CI は Phase 3 に送る。

## 課題 1: CSV を「移行用 CSV」として完成させる

現在の CSV は文字列を `"..."` で囲んだ素朴な形式。Phase 2 で TiDB Lightning や
`mysql --local-infile` から取り込めるように整える。

### サーバ側 cast による型の文字列化

PG カタログから取り出した `data_type` / `udt_name` を見て、`buildSelectQuery` が
**SELECT 句で型ごとに cast を仕込む**:

| PG 型 | SELECT 句に入る式 | CSV セル |
|---|---|---|
| `timestamptz` | `to_char(col AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US')` | `2026-04-26 12:00:00.000000` |
| `timestamp` | `to_char(col, 'YYYY-MM-DD HH24:MI:SS.US')` | 同上(TZ 無し) |
| `date` | `to_char(col, 'YYYY-MM-DD')` | `2026-04-26` |
| `time`、`timetz` | `to_char(col, 'HH24:MI:SS.US')` | `12:00:00.000000` |
| `interval` | `EXTRACT(EPOCH FROM col)::numeric` | 秒数(浮動小数) |
| `bool` | `(col)::int` | `0` / `1` |
| `bytea` | `encode(col, 'hex')` | 16 進文字列(MySQL の `UNHEX(col)` 列に投入可) |
| `uuid` | `(col)::text` | `xxxx-xxxx-...` |
| `json`、`jsonb` | `(col)::text` | JSON 文字列 |
| `inet`、`cidr`、`macaddr` | `(col)::text` | 文字列 |
| 配列(`_int4` 等) | `to_jsonb(col)::text` | JSON 配列 |
| range / multirange | `to_jsonb(col)::text` | JSON `{"l":..,"u":..,"li":..,"ui":..}` |
| ユーザ定義 enum | `(col)::text` | ラベル文字列 |
| `tsvector`、`tsquery` | `(col)::text` | PG リテラル形式(検索性は失う) |
| `numeric` | そのまま | 文字列(精度を保つ) |

実装:

- `export/sql.go` の `buildSelectQuery` を `buildPgSelectExpr(colName, dataType, udtName)` 経由に。
- 型情報は `information_schema.columns` から `data_type` と `udt_name` を取って
  `meta TableMeta` に積む(現状 `colTypes` は `*sql.ColumnType` のみ)。
- pgx の `*sql.ColumnType.DatabaseTypeName()` でも分かるが、配列 / range の
  細部は `udt_name` から起こす方が確実。

### CSV のオプション整理

- **ヘッダ**: 既定 ON、`--no-header` で OFF(現状維持)
- **delimiter / separator / line terminator**: 現状維持
- **NULL 表記**: 既定 `\N`、Lightning 互換のため `--csv-null-value` で変更可(現状維持)
- **新規: `--csv-binary-format {hex,base64,raw}`**: bytea/binary 表現の切替(既定 `hex`)。
  raw は CSV 内に生バイトを置くので escape の責任が重く非推奨だが互換のため残す。
- **新規: `--csv-zero-based-bool`**: bool を `t/f` ではなく `1/0` で書く(既定有効、TiDB Lightning 向け)。

### CSV 検証(Phase 2 完了条件)

```sh
docker run -d --name pg postgres:17 ...
docker run -d --name tidb pingcap/tidb:nightly ...   # or use Zero
psql -d demo -f - <<SQL
CREATE TABLE t (
  id int primary key,
  s text, j jsonb, ts timestamptz, b bytea, u uuid, arr int[]
);
INSERT INTO t VALUES (1, 'hello', '{"a":1}', now(), '\xDE\xAD',
                      gen_random_uuid(), ARRAY[1,2,3]);
SQL
pg-dumpling ... --filetype csv -o /tmp/csv
# Lightning で取り込み
tidb-lightning --backend tidb --data-source-dir /tmp/csv --... 
# row count + 全カラム照合
```

## 課題 2: Parquet 出力

`--filetype parquet` を追加。dumpling の writer pipeline を再利用しつつ、
serializer だけ Parquet writer に差し替える。

### ライブラリ選定

第一候補: `github.com/apache/arrow-go/v18`(`replace` 経由で TiDB go.mod から既に
読み込まれているので追加負荷ゼロ)。

代替案: `github.com/parquet-go/parquet-go`(arrow 依存なし、軽量)。Phase 2 で
評価して決める。

### スキーマ導出

`information_schema.columns` + `pg_type` の情報から Parquet schema を構築:

| PG 型 | Parquet 物理型 | logical type | 備考 |
|---|---|---|---|
| `int2` / `int4` | INT32 | INT(16/32, signed=true) | |
| `int8` | INT64 | INT(64, signed=true) | |
| `float4` | FLOAT | — | |
| `float8` | DOUBLE | — | |
| `numeric(p,s)` | FIXED_LEN_BYTE_ARRAY 等 | DECIMAL(p,s) | precision 不明時は STRING にフォールバック |
| `bool` | BOOLEAN | — | |
| `text` / `varchar` / `char` / `name` | BYTE_ARRAY | STRING(UTF8) | |
| `bytea` | BYTE_ARRAY | — | バイナリそのまま |
| `uuid` | FIXED_LEN_BYTE_ARRAY(16) | UUID | 16 バイト直値 |
| `date` | INT32 | DATE | エポック日数 |
| `time(tz)` | INT64 | TIME(MICROS, isAdjustedToUTC) | TZ は UTC 化 |
| `timestamp` | INT64 | TIMESTAMP(MICROS, isAdjustedToUTC=false) | |
| `timestamptz` | INT64 | TIMESTAMP(MICROS, isAdjustedToUTC=true) | UTC 化済 |
| `interval` | INT64(秒) | — or BYTE_ARRAY(STRING) | 設計時に確定 |
| `inet` / `cidr` / `macaddr` | BYTE_ARRAY | STRING | |
| `json` / `jsonb` | BYTE_ARRAY | JSON | |
| 配列・range・enum | BYTE_ARRAY | JSON | server-side `to_jsonb(col)::text` で取得 |

NULL は Parquet 標準のリピート/optional レベルで表現。

### ストリーミング書き出し

`writer_util.go` のチャンク境界(`--filesize`)に合わせて Parquet ファイルを切る。
1 ファイル = 1 row group としてシンプルに開始し、性能課題が出たら row group 単位
に細分化する。圧縮は `--compress {snappy,zstd,gzip}` を Parquet codec に
マッピング(既定 `snappy`)。

### 検証(Phase 2 完了条件)

```sh
pg-dumpling ... --filetype parquet -o /tmp/pq
parquet-tools schema /tmp/pq/public.t.000000000.parquet
parquet-tools cat   /tmp/pq/public.t.000000000.parquet | head
# TiDB Lightning に Parquet 取り込み(対応バージョン要確認)
tidb-lightning --backend tidb --data-source-dir /tmp/pq ...
```

## 課題 3: テーブル内 chunking の復活

Phase 1 で `getNumericIndex` を空文字返却にした穴を塞ぐ。

1. **数値 PK レンジ chunk**(優先度高)
   - `pg_index` で PK / unique 取得 → `pg_attribute` 経由で型確認 → 整数型なら採用
   - 既存の `getMinMax` / `splitTableWithIndex` をクォート修正だけで流用
   - `--rows N` フラグの no-op を解除

2. **`ctid` レンジ chunk**(PK 無しテーブル向け、PG 特有)
   - `pg_relation_size / current_setting('block_size')::int` でブロック数を推定
   - `WHERE ctid >= '(s,0)' AND ctid < '(e,0)'` で WHERE 構築
   - `pg_dump --jobs N` と同じ手法

3. **partition 単位 fan-out**
   - `GetPartitionNames` は Phase 1 で既に PG 用に書き直し済み
   - `concurrentDumpTable` から partition ごとに dump task 発行する経路を復活

## 課題 4: `replace ~/Work/tidb` の解消

`go.mod` から `replace github.com/pingcap/tidb => ...` を消すには現在依存している
以下を `internal/` に取り込むか書き直す:

| 依存 | 用途 | 取り込み方針 |
|---|---|---|
| `br/pkg/storage` | S3/GCS/Azure 抽象 | そのままコピー(Apache 2.0) |
| `br/pkg/utils` | retry helper | `WithRetry` 1 関数のみ自前実装で代替 |
| `br/pkg/summary` | progress 集計 | コピー or zap log 化で簡素化 |
| `pkg/util/table-filter` | `--filter` 解析 | コピー(独立性高) |
| `pkg/util/promutil` | metrics ファクトリ | コピー(数行) |
| `pkg/util` | `SliceKeysQuoted` 等 | 必要関数のみ inline |

これで完全独立モジュールになり、外部に push 可能になる。

## 着手順序

1. **04a — CSV ハードニング**: 型ごとの SELECT 句 cast、`--csv-binary-format`
   等のオプション、テストデータでの round-trip 確認
2. **04b — Parquet バックエンド**: `--filetype parquet` のスキーマ導出 + writer
3. **04c — テーブル内 chunk 復活**: 数値 PK → ctid → partition fan-out
4. **04d — TiDB 依存解消**: `replace` 削除、`internal/` 取り込み、`go mod tidy`
5. **04e — ドキュメント整備とリリース**: 移行ガイド、型マトリクス、`v0.4.0` タグ

CI(GitHub Actions)、PostGIS / hstore / tsvector の特殊型対応は Phase 3 へ送る。

## 非ゴール(Phase 2 ではやらない)

- MySQL 互換 SQL 出力モード(`--target=mysql/tidb` フラグ案は廃案)
- スキーマ DDL の自動 PG → TiDB 変換(LLM 任せ)
- 完全な PG 機能カバレッジ(PostGIS、hstore、tsvector、ユーザ定義型 — 制約として
  ドキュメント化のみ)
- パブリケーション・トリガ・ストアドプロシージャの dump
- 双方向同期(あくまでも一方向)
- CI / GitHub Actions(Phase 3)

## 関連: 既存の SQL 出力モードの位置づけ

現状の `--filetype sql`(PG-flavored INSERT)は **そのまま残す**:

- 用途は「同じ PG クラスタへの round-trip 検証」「小規模 dev 用」「デバッグ」
- ローダから `psql -f` で素直に流せるので CSV/Parquet が動かない環境でも使える
- ただし migration には公式に推奨しない、と README に明記する
