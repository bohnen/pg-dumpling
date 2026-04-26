# 04 — Phase 2 roadmap: 中間フォーマットによる PG → TiDB 移行

- 日付: 2026-04-26
- 担当: tadatoshi.sekiguchi@pingcap.com (with Claude)
- 状態: 計画。実装着手前

## 方針(2026-04-26 確定)

Phase 2 は **CSV を中間フォーマット**として整備し、ローダ(TiDB Lightning 等)
経由で TiDB / MySQL に取り込む経路を実用化することをゴールとする。

直接ロード可能な「MySQL 互換 SQL」を吐く方向は **採用しない**:

- PG ↔ MySQL の非互換は型・関数・SQL_MODE・トランザクション・予約語・大文字小文字・
  TZ 取り扱いに渡って多岐にわたり、`SET …` 一発では揃わない
- 中間フォーマット(CSV)なら型変換ロジックがシンプル(serializer 1 箇所に集中)で、
  ローダ側のバグ修正・最適化の恩恵を受けられる
- TiDB Lightning は CSV をネイティブに受けられる

`--target=postgres` フラグは不要(用途は `pg_dump` でカバー済み)。SQL 出力モードは
小規模 round-trip / デバッグ用として現状を維持するが、移行用途では使わない。

**Parquet は対象外**(上流 dumpling もサポートしておらず、本リポジトリで新規導入
する優先度は低い)。必要になったら Phase 3 以降で再検討。

## ゴール

1. **CSV を TiDB Lightning に流せる品質に**(PG 固有型の安全な文字列化、
   `\N` NULL、適切なバイナリ表現)
2. **テーブル内 chunking 復活**(数値 PK レンジ / ctid レンジ / partition 子表)
3. **`replace ~/Work/tidb` の解消**(完全独立モジュール化)
4. ドキュメント整備と `v0.4.0` リリース

CI と Parquet 出力は Phase 3 以降に送る。

## 課題 1: CSV を「移行用 CSV」として完成させる

現在の CSV は文字列を `"..."` で囲んだ素朴な形式。Phase 2 で TiDB Lightning や
`mysql --local-infile` から取り込めるように整える。

### 時刻精度・タイムゾーンの扱い(MySQL/TiDB 移行の主要な落とし穴)

参考: <https://zenn.dev/bohnen/articles/pgvector-to-tidb-migration>。実例として
`TIMESTAMPTZ '2026-03-29 13:59:13.505439+00'` を MySQL の `DATETIME` に直接
入れようとして失敗するケースが報告されている。pg-dumpling が CSV 出力時に必ず
正規化すべきポイント:

- **タイムゾーン情報を出力に残さない**。MySQL/TiDB の `DATETIME` / `TIMESTAMP`
  は文字列リテラルに `+00:00` 等のオフセットを含めるとパースエラーになる。
  `timestamptz` は **UTC に正規化してから TZ を捨てる**(`AT TIME ZONE 'UTC'`)。
- **小数秒は最大 6 桁(マイクロ秒)に揃える**。
  - PG の物理精度: `timestamp` / `timestamptz` は 8 byte int の **マイクロ秒**
    (1 µs = 1000 ns)。
  - Go `time.Time` は **ナノ秒**精度。`pgx` の標準テキスト化や `time.Format(time.RFC3339Nano)`
    を経由すると `2026-03-29 13:59:13.505439123` のように 9 桁出ることがある。
  - MySQL / TiDB の `DATETIME(p)` / `TIMESTAMP(p)` は `p ≤ 6`(マイクロ秒)が上限。
    7 桁以上を含む文字列は **`Incorrect datetime value`** で取込失敗。
  - → pg-dumpling は **常に 6 桁にトリム**して出力する(7 桁目以降は捨てる)。
    `to_char(col, 'YYYY-MM-DD HH24:MI:SS.US')` を使えば PG 側で 6 桁固定にできる
    ので、Go `time.Time` の RFC3339Nano フォーマットを通さない設計にする。
- **`time` / `timetz` も同様**: TZ 付きは UTC 化、小数秒 6 桁。
- **`date` は精度・TZ 議論なし**。`'YYYY-MM-DD'` で OK。
- **`interval`**: MySQL/TiDB に対応型がないので **秒数(浮動小数)** か文字列に
  変換。`EXTRACT(EPOCH FROM col)::numeric` で秒数化が無難(後段で `INT` / `BIGINT`
  / `DECIMAL` 列に入れる)。
- **negative timestamp**: PG は紀元前を扱えるが MySQL の `DATETIME` は `1000-01-01`
  以降のみ。範囲外データはダンプ時に WARN ログを出して素通しする(ロード側で弾く)。

### サーバ側 cast による型の文字列化

PG カタログから取り出した `data_type` / `udt_name` を見て、`buildSelectQuery` が
**SELECT 句で型ごとに cast を仕込む**。Go `time.Time` を介さず文字列で受け取る
ことで時刻のナノ秒問題を根本から避ける:

| PG 型 | SELECT 句に入る式 | CSV セル | 取込先(TiDB)目安 |
|---|---|---|---|
| `timestamptz` | `to_char(col AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US')` | `2026-04-26 12:00:00.000000` | `DATETIME(6)` or `TIMESTAMP(6)` |
| `timestamp` | `to_char(col, 'YYYY-MM-DD HH24:MI:SS.US')` | 同上(TZ 無し) | `DATETIME(6)` |
| `date` | `to_char(col, 'YYYY-MM-DD')` | `2026-04-26` | `DATE` |
| `time` | `to_char(col, 'HH24:MI:SS.US')` | `12:00:00.000000` | `TIME(6)` |
| `timetz` | `to_char(col AT TIME ZONE 'UTC', 'HH24:MI:SS.US')` | UTC 正規化済 | `TIME(6)` |
| `interval` | `EXTRACT(EPOCH FROM col)::numeric` | 秒数(浮動小数) | `DECIMAL` or `BIGINT` |
| `bool` | `(col)::int` | `0` / `1` | `TINYINT(1)` |
| `bytea` | `encode(col, 'hex')` | 16 進文字列 | `BINARY` / `VARBINARY` / `BLOB`(`UNHEX(col)` 列指定で投入) |
| `uuid` | `(col)::text` | `xxxx-xxxx-...` | `CHAR(36)` or `BINARY(16)` |
| `json`、`jsonb` | `(col)::text` | JSON 文字列 | `JSON` |
| `inet`、`cidr`、`macaddr` | `(col)::text` | 文字列 | `VARCHAR` |
| 配列(`_int4` 等) | `to_jsonb(col)::text` | JSON 配列 | `JSON` |
| range / multirange | `to_jsonb(col)::text` | JSON `{"l":..,"u":..,"li":..,"ui":..}` | `JSON` |
| ユーザ定義 enum | `(col)::text` | ラベル文字列 | `VARCHAR` or `ENUM(...)` |
| `tsvector`、`tsquery` | `(col)::text` | PG リテラル形式(検索性は失う) | `TEXT`(検索用途は別途要設計) |
| `numeric` | そのまま | 文字列(精度を保つ) | `DECIMAL(p,s)` |
| `pgvector`(`vector`) | `(col)::text` | `[1,2,3]` | TiDB `VECTOR` 列に投入可(角括弧形式互換) |

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

## 課題 2: テーブル内 chunking の復活

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

## 課題 3: `replace ~/Work/tidb` の解消(進行中)

### 04c で完了した削減

7 パッケージ → **1 パッケージ**に縮小:

| パッケージ | 04c 後の状態 |
|---|---|
| `br/pkg/version` | ❌ 削除(`Config.ServerInfo` 自体撤廃) |
| `pkg/util/promutil` | ❌ stdlib `prometheus.Registerer` 直叩きに置換 |
| `br/pkg/utils.WithRetry` | ❌ `retry.go` に inline(~30 行) |
| `br/pkg/summary` | ❌ zap log 呼び出しに置換 |
| `pkg/util` (TLS) | ❌ stdlib `crypto/tls` + `crypto/x509` に書き換え |
| `pkg/util/table-filter` | ✅ `internal/table-filter/` に vendor(`pingcap/errors` のみ依存) |
| `br/pkg/storage` | 🔧 **未対応**(transitive deps 多数) |

### `br/pkg/storage` を取り込むには

単純コピーではダメで、storage が以下を更に要求する:

- `br/pkg/errors`、`br/pkg/logutil`、`br/pkg/utils`、`br/pkg/utils/iter`
- `pkg/util`、`pkg/util/intest`、`pkg/util/logutil`、`pkg/util/prefetch`
- `pkg/sessionctx/variable`、`pkg/lightning/log`

**確定方針(2026-04-26)**: S3/GCS/Azure/HDFS/ks3 は dumpling の魅力の一つなので
**温存**。Phase 2 では `replace ~/Work/tidb` を残し、Phase 3 で **public TiDB
commit への pin**(`require github.com/pingcap/tidb v0.0.0-<sha>`)で `replace`
を解消する。

`go mod download` 時に TiDB ツリー全体が落ちてくる代わりに、build 後は linker が
unused symbol を捨てるので `bin/pg-dumpling` のサイズに目立った影響は無い見込み。

vendor 案(transitive deps を `internal/` に全部取り込む)は storage の依存深度を
考えると割に合わないので非採用。
## 着手順序と完了状況

1. ✅ **04a — CSV ハードニング**(commit `030e7c4`): `pgMigrationCast` で 21 種の
   PG 型に SELECT 句 cast。Round-trip 検証成功(MySQL / TiDB 両方)
2. ✅ **04b — テーブル内 chunk 復活**(commit `dcad158`): `getNumericIndex` 復活、
   `concurrentDumpTableByCtid` 新設、`pg_class.reltuples` 行数推定に切替
3. ✅ **04c — TiDB 内部依存削減**(commits `7035f3f` + `42dd13a`): 7 → 1 パッケージ
   に縮小。`replace ~/Work/tidb` 自体は `br/pkg/storage` のために Phase 3 まで残置
4. 🔧 **04d — ドキュメント整備とリリース**: 本コミット。`v0.4.0` タグを打つ

CI(GitHub Actions)、`replace` 完全解消、Parquet 出力、PostGIS / hstore / tsvector
の特殊型対応は Phase 3 以降。

## 移行先(TiDB 側)で利用者が手当てする項目

pg-dumpling は **データの中間化**に集中し、以下のような DDL レベルの違いは **ユーザ
側 / LLM での DDL 変換**に委ねる(参考記事の知見):

- `BIGINT GENERATED ALWAYS AS IDENTITY` → `BIGINT AUTO_INCREMENT`(PG の
  IDENTITY 構文は MySQL/TiDB に存在しない)
- `TEXT` + `UNIQUE` → `VARCHAR(255)` + `UNIQUE KEY` または **prefix length 指定**
  (TiDB は無制限 TEXT への UNIQUE インデックスを許容しない)
- `pgvector` の `vector(N)` → TiDB `VECTOR(N)`(角括弧テキスト形式は互換あり)
- 外部キー名・インデックス名の重複ルール、CHECK 制約の差異
- `GENERATED ALWAYS AS (...) STORED` 計算列(MySQL/TiDB は構文が近いが互換性確認必要)

`<table>-schema.sql` は `pg_dump --schema-only` 出力をそのまま入れている(Phase 1
で確定済の方針)ため、利用者は LLM 等で TiDB DDL に変換してから実行する。
README に変換ガイド・プロンプト例を載せるのが Phase 2 の docs スコープ。

## ローダ側の TIPS

### bytea を BLOB / VARBINARY に復元する(最重要)

`LOAD DATA INFILE` は CSV セルから直接 BLOB / VARBINARY にバイナリを流せない
(値に改行・引用符・NULL バイトが含まれているとフィールド/レコード境界が壊れる)。
pg-dumpling は PG 側で `encode(col, 'hex')` を呼んで **ASCII セーフな 16 進文字列**
として CSV に書き出すので、ローダ側で **`@var` 経由 + `SET = UNHEX(@var)`** で
逆変換するのが標準。

```sql
LOAD DATA LOCAL INFILE '/path/public.bin_test.000000000.csv'
INTO TABLE bin_test
FIELDS TERMINATED BY ',' OPTIONALLY ENCLOSED BY '"' LINES TERMINATED BY '\r\n'
IGNORE 1 LINES
(id, @small, @many, @empty, @null_)
SET small_bin = UNHEX(@small),
    many_bin  = UNHEX(@many),
    empty_bin = UNHEX(@empty),
    null_bin  = UNHEX(@null_);
```

NULL は `\N` のまま素通りする(`UNHEX(NULL) = NULL`)。空 bytea は 0 バイトの
BLOB として復元される。改行(`0x0A`)・引用符(`0x22`)・バックスラッシュ
(`0x5C`)・NULL バイト(`0x00`)を含む 8 バイトのテストデータでも、TiDB
v8.5.6 で `HEX()` 確認まで含めて完全復元することを確認済(2026-04-26)。

### その他

- **ヘッダ行**: `IGNORE 1 LINES` でスキップ(`--no-header` 無しが既定)
- **時刻列**: pg-dumpling 側の cast で `YYYY-MM-DD HH24:MI:SS.US` 形式にして
  あるので `DATETIME(6)` / `TIMESTAMP(6)` カラムへ直接 INSERT で OK。
  値が独自形式の場合は `SET ts = STR_TO_DATE(@col, '%Y-%m-%d %H:%i:%s.%f')`
  を噛ませる選択肢もある
- **JSON 列**: pg-dumpling は `(col)::text` / `to_jsonb(col)::text` で出力する
  ので、TiDB の `JSON` 型カラムへ直接 INSERT 可
- **UUID**: 36 文字文字列で来るので `CHAR(36)` または `VARCHAR(36)` が素直。
  `BINARY(16)` に詰めたい場合は `SET uid = UNHEX(REPLACE(@uid_str,'-',''))`
- **pgvector(`vector(N)`)**: `[1.0,2.0,3.0]` の角括弧形式は TiDB の
  `VECTOR(N)` 型に直接 INSERT 可

## 非ゴール(Phase 2 ではやらない)

- MySQL 互換 SQL 出力モード(`--target=mysql/tidb` フラグ案は廃案)
- スキーマ DDL の自動 PG → TiDB 変換(LLM 任せ)
- 完全な PG 機能カバレッジ(PostGIS、hstore、tsvector、ユーザ定義型 — 制約として
  ドキュメント化のみ)
- パブリケーション・トリガ・ストアドプロシージャの dump
- 双方向同期(あくまでも一方向)
- CI / GitHub Actions(Phase 3)
- Parquet 等の追加フォーマット出力(上流 dumpling もサポートしていない)

## 関連: 既存の SQL 出力モードの位置づけ

現状の `--filetype sql`(PG-flavored INSERT)は **そのまま残す**:

- 用途は「同じ PG クラスタへの round-trip 検証」「小規模 dev 用」「デバッグ」
- ローダから `psql -f` で素直に流せるので CSV/Parquet が動かない環境でも使える
- ただし migration には公式に推奨しない、と README に明記する
