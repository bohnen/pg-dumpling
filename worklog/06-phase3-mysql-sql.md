# Step 06 — Phase 3: SQL 出力を MySQL/TiDB 向けに修正

## ゴール

`--filetype=sql --target=mysql` (or `tidb`) で出力したファイルを `mysql -f`、
`tidb -f` で直接ロードできるようにする。CSV (Lightning ルート) と並ぶ第二の
移行経路を提供する。

CSV モードで作った `pgMigrationCast()`(21 型の server-side cast)を SQL モード
でも流用するのが最大のテコ。値整形はほぼ流用、識別子クォートとプリアンブル
だけ MySQL/TiDB 風に切り替える。

## 確定済の設計判断(ユーザー承認: 2026-04-26)

| 項目 | 決定 |
|---|---|
| `--target` デフォルト | `mysql`(本プロジェクトの本来目的に合致。PG round-trip は `--target=pg` で明示) |
| schema → database マッピング | そのまま `` `public`.`t` `` を出す。複数スキーマ衝突を避けるため |
| `--target=tidb` の枝分かれ | 当面 mysql と同一出力。差異が出たら分岐 |
| DDL 変換 | スコープ外。`<table>-schema.sql` は引き続き PG ネイティブ。LLM で別途変換 |

## 主な技術課題と方針

| 領域 | 現状(PG) | 修正後(MySQL/TiDB) |
|---|---|---|
| 識別子クォート | `"schema"."table"`、`"col"` | `` `schema`.`table` ``、`` `col` `` |
| プリアンブル | `SET standard_conforming_strings/client_encoding/search_path` | `SET NAMES utf8mb4; SET FOREIGN_KEY_CHECKS=0; SET UNIQUE_CHECKS=0; SET sql_mode='NO_BACKSLASH_ESCAPES'` |
| 文字列リテラル | `'O''Brien'`(SQL 標準) | 同左(`NO_BACKSLASH_ESCAPES` 有効化で素通し) |
| bytea | `'\x48656c6c6f'`(PG hex literal) | `X'48656C6C6F'`(MySQL hex literal) |
| timestamptz / interval / array / inet / json / uuid 等 | PG ネイティブ表現 | `pgMigrationCast()` を SQL モードでも有効化 → MySQL 互換文字列 |
| INSERT 文 | `INSERT INTO "s"."t" VALUES (...)` | `` INSERT INTO `s`.`t` VALUES (...) `` |

## CLI

```
--target {mysql, tidb, pg}    SQL 出力方言。デフォルト mysql
```

- `--target=mysql/tidb`: 当面同一出力(差異が出たら分岐)
- `--target=pg`: 旧来の PG round-trip 用出力(後方互換)
- `--filetype=csv` のときは `--target` は no-op(CSV は既に MySQL/TiDB 互換)

## 実装ステップ

### Step 06a — Dialect 抽象化 + `--target` フラグ(コード変更のみ、出力は不変)

- `export/dialect.go` 新設
- `Dialect` interface:
  ```go
  type Dialect interface {
      Name() string
      QuoteIdent(name string) string
      QuoteQName(schema, table string) string
      Preamble() string
      BytesLiteral(b []byte) string  // SQL 文字列としてのバイト列リテラル
      WantMigrationCasts() bool
  }
  ```
- `pgDialect` / `mysqlDialect` 実装
- `Config.Target` enum 追加(`TargetPg`, `TargetMySQL`, `TargetTiDB`)
- `--target` フラグ登録、デフォルト `pg`(Step 06a の段階では出力を変えない)
- 既存 `pgQuoteIdent` / `pgQuoteQName` 呼出を `cfg.Dialect().QuoteIdent(...)` 経由に置換
- writer_util.go の preamble / INSERT / bytea 書き出しを Dialect ディスパッチへ
- ビルド・単体テスト通過

### Step 06b — mysql 方言の値整形(出力が変化)

- `mysqlDialect`: backtick クォート、MySQL preamble、`X'…'` BYTEA
- `target != pg` のとき `buildSelectField(migrationCasts=true)` を SQL モードでも有効化
- 文字列受信器が `pgMigrationCast` 出力をそのまま `'…'` で囲む(quote-doubling 適用)
- `--target` のデフォルトを `mysql` に切替
- 単体テストで pg / mysql の出力差を検証

### Step 06c — e2e 検証 + ドキュメント + v0.6.0

- Docker で `postgres:17` から `--filetype=sql --target=mysql` で dump
- 21 型 allty テーブル + bytea + 複数チャンク
- `mysql:8.4` と `tidb:v8.5.6` の両方で `mysql -h … < dump.sql` が成功し、
  件数 + 値が一致
- CLAUDE.md(マイルストーン表 + 出力方言セクション)、README、本ファイル更新
- `v0.6.0` タグ

## Verification(最終目標)

```sh
# DDL は手動翻訳(スコープ外)
mysql -e "CREATE DATABASE demo"
mysql demo < hand_translated_schema.sql

# 本作業の成果物
./bin/pg-dumpling -h pg -B demo -o /tmp/dump --filetype sql --target mysql
mysql demo < /tmp/dump/demo.t.000000000.sql
mysql -e "SELECT count(*), sum(crc32(s)) FROM demo.t"
# => 件数・チェックサムが PG 側と一致
```

## スコープ外(本ステップでやらない)

- DDL 変換(`pg_dump --schema-only` 出力の MySQL/TiDB 化)
- PostGIS / hstore / tsvector などの特殊型(Phase 4)
- CI(Phase 4)
- Parquet 出力(Phase 4)

## マイルストーン後の状態

- v0.4.0: PG → MySQL/TiDB via CSV(Lightning ルート)
- v0.5.0: replace ~/Work/tidb 撤去
- **v0.6.0: PG → MySQL/TiDB via SQL(`mysql -f` 直叩きルート)**

「どちらのルートでも移行できる」プロジェクトとして完結する。

## 実装結果(2026-04-26 完了)

### コミット内訳

- **Step 06a**: `export/dialect.go` 新設(SQLDialect interface、pg/mysql 実装、SQLTarget enum、Config.Dialect()、`--target` フラグ)。コード変更だけで出力は変化させず、ビルド・unit test 通過
- **Step 06b**:
  - `pgMigrationCast` に `csvMode bool` 引数追加。SQL モードでは bytea を encode せず raw bytes として渡す
  - `buildSelectField` を `(selectExpr, selectColumns string, n int, err error)` 返却に変更。SELECT 式と INSERT カラムリストを分離
  - `tableMeta.selectColumns` 追加、`SelectColumns()` getter 公開
  - `MakeRowReceiver` / `SQLTypeBytes*Maker` に dialect を渡す
  - `SQLTypeBytes` が dialect.BytesLiteral でリテラル化
  - `writer_util.go` の INSERT prefix を `dialect.QuoteQName` + `meta.SelectColumns()` ベースに変更
  - `getSpecialComments` / `pgFilePreamble` を撤去、`dialect.Preamble()` に統合
  - `ShowCreateDatabase` を dialect ディスパッチに変更
  - 既定値を `--target=mysql` に切替

### Verification 結果

PG セットアップ:

```sql
CREATE TABLE allty (id serial PK, s_text text, b_bytea bytea, ts_tz timestamptz,
  ts_notz timestamp, d_date date, t_time time, t_timetz timetz, intvl interval,
  flag bool, uuid_v uuid, json_v json, jsonb_v jsonb, inet_v inet,
  arr_int int[], arr_txt text[]);
INSERT INTO allty VALUES (..., 'hello', E'\\x48656c6c6f00ff', ...), (... 'O''Brien' ...);
```

`pg-dumpling --target=mysql` の出力(抜粋):

```sql
/*!40101 SET NAMES utf8mb4 */;
/*!40014 SET FOREIGN_KEY_CHECKS=0 */;
/*!40014 SET UNIQUE_CHECKS=0 */;
SET sql_mode='NO_BACKSLASH_ESCAPES';
INSERT INTO `public`.`allty` (`id`,`s_text`,...,`b_bytea`,`ts_tz`,...,`arr_txt`) VALUES
(1,'hello',...,X'48656C6C6F00FF','2026-01-15 10:30:45.123456',...,'["a", "b''c"]'),
(2,'O''Brien',NULL,...,NULL);
```

`mysql -h … <` での直叩きロード:

| ターゲット | 結果 |
|---|---|
| `mysql:8.4` | ✅ DDL 手動翻訳 → INSERT 直接ロード成功。bytea(`HEX(b_bytea)` で `48656C6C6F00FF` 一致)、quote-doubling、JSON 配列、interval、すべて意図通り |
| `tidb:v8.5.6` | ✅ 同 DDL で同手順、すべて一致 |

`--target=pg` も維持されており、PG round-trip 出力(`'\x48656c6c6f00ff'`、`"public"."allty"`、PG preamble)を確認。

### 確定した PG schema → MySQL DB マッピング

PG の `public` schema は MySQL の `` `public` `` データベースとして出る。手動 DDL 翻訳時にも `CREATE DATABASE \`public\`; USE \`public\`;` の形で受ければそのまま動く。複数 schema を持つ PG DB から複数 MySQL DB に分かれてマップされる(衝突しない)。

### コミット履歴

- `<TBD>` Step 06a: SQLDialect 抽象化 + `--target` フラグ
- `<TBD>` Step 06b: mysql 方言の値整形 + デフォルト切替
- `<TBD>` Step 06c: ドキュメント + v0.6.0 タグ

(タグ付け時に埋める)
