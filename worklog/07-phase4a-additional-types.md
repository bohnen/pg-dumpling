# Step 07 — Phase 4a: PG 追加型対応

## 背景

v0.6.0 (Phase 3) の `pgMigrationCast()` がカバーする型は 21 種。実プロジェクト
で遭遇しうるが未対応の型がいくつかあったため、組み込み型と一般的な extension
(hstore)を補う。PostGIS のような外部依存・GIS 特化の型は別 Phase。

## 追加した型(計 11 種)

| 型 | dataType | udtName | 採用した cast | 備考 |
|---|---|---|---|---|
| `bit(n)` | `bit` | `bit` | `(col)::text` | リテラル文字列 `'10101010'` |
| `bit varying(n)` | `bit varying` | `varbit` | `(col)::text` | 同上 |
| `point` | `point` | `point` | `(col)::text` | `(1,2)` 形式 |
| `line` | `line` | `line` | `(col)::text` | `{A,B,C}` 形式(`Ax+By+C=0`) |
| `lseg` | `lseg` | `lseg` | `(col)::text` | `[(x1,y1),(x2,y2)]` |
| `box` | `box` | `box` | `(col)::text` | PG が `(maxx,maxy),(minx,miny)` に正規化する点に注意 |
| `path` | `path` | `path` | `(col)::text` | open `[...]` / closed `(...)` の両方 |
| `polygon` | `polygon` | `polygon` | `(col)::text` | `((x1,y1),(x2,y2),...)` |
| `circle` | `circle` | `circle` | `(col)::text` | `<(x,y),r>` |
| `hstore` | `USER-DEFINED` | `hstore` | `hstore_to_jsonb(col)::text` | `--target=mysql/tidb` 時のみ JSON 化、`--target=pg` では native `"k"=>"v"` を維持 |
| 複合型 (composite) | `USER-DEFINED` | (任意) | `(col)::text`(フォールスルー) | PG タプル形式 `(a,b,c)` をそのまま文字列として出力。MySQL JSON 化したい場合はユーザー側で `to_jsonb` ビューを噛ませる |

## 設計判断

### geometric 型はすべて `::text`

PG の組み込み 2D geometric 型(`point`/`line`/`lseg`/`box`/`path`/`polygon`/
`circle`)は MySQL/TiDB に意味的に対応する型がない。WKT/WKB ではない PG 独自
形式だが、文字列として保存すれば round-trip 可能。GIS 用途なら PostGIS+`ST_AsText`
で別途扱う(Phase 4 残課題)。

### hstore は target に応じて分岐

`hstore` のデフォルト `::text` は `"k"=>"v"` という PG 専用形式で MySQL JSON
列に直接は入らない。Phase 4a では `pgMigrationCast` 内で hstore 専用に
`hstore_to_jsonb()::text` を適用するように変更。

これは migration cast が ON のとき(`--target=mysql/tidb` の SQL モード、または
任意 target の CSV モード)のみ効く。`--target=pg --filetype=sql` では
migration cast 自体が走らないので、hstore は PG native のまま round-trip する
(検証済)。

### 複合型は手当てしない

composite type の `::text` は `(a,b,c)` 形式で出る。`to_jsonb()` を当てれば JSON
化できるが、`pgMigrationCast` は `dataType='USER-DEFINED'` から composite かどうか
を判別できない(`pg_type.typtype='c'` を引くには追加の catalog query が必要)。
頻度が低いため、フォールスルーの `::text` で文字列出力にとどめ、ユーザーが必要
なら schema 側で工夫する想定。

### bit 系は文字列扱い

PG の `bit(n)` は `bit` という独立した型だが、その text 形式は `'10101010'` の
ようなリテラル。MySQL の BIT 列に直接入れると意図と違う(MySQL は数値表現)。
本ステップではユーザーが MySQL 側で `VARCHAR(n)` 列を用意すること前提で文字列
化する。

## Verification(2026-04-27 完了)

### 入力

```sql
CREATE EXTENSION hstore;
CREATE TYPE addr AS (street text, city text, zip int);
CREATE TABLE newtypes (
  id serial PK,
  b1 bit(8), b2 bit varying(16),
  pt point, ln line, lg lseg, bx box, pa path, py polygon, ci circle,
  hs hstore, ad addr
);
INSERT INTO newtypes VALUES (..., B'10101010', B'1101', '(1,2)', ..., 
  hstore('color','blue') || hstore('special', 'it''s'),
  ROW('123 Main','SF',94110)::addr), (NULL × 11);
```

### `--target=mysql` 出力(抜粋)

```sql
INSERT INTO `public`.`newtypes` (`id`,`b1`,`b2`,`pt`,...,`hs`,`ad`) VALUES
(1,'10101010','1101','(1,2)',...,
 '{"size": "M", "color": "blue", "special": "it''s"}',
 '("123 Main",SF,94110)'),
(2,NULL,NULL,NULL,...,NULL,NULL);
```

`mysql:8.4` に直接ロード成功。`hs->>'$.color'` で `blue` を抽出可能、
`it's` の quote-doubling も正しくデコード。

### `--target=pg` 出力(抜粋)

```sql
INSERT INTO "public"."newtypes" VALUES
(1,'10101010','1101','(1,2)',...,
 '"size"=>"M", "color"=>"blue", "special"=>"it''s"',
 '("123 Main",SF,94110)'),
```

PG round-trip(別 DB に同じ extension/type を作成してから流し込み)で
件数 + 全カラム値が完全一致。

## 現状カバー状況一覧

ここまでで `pgMigrationCast` がカバーする PG 型の総覧:

### 単純型(20 種)

数値: `int2/4/8`, `float4/8`, `numeric`, `money`, `oid/xid/cid/tid` (10)
真偽: `boolean`
バイナリ: `bytea`
文字: `text`, `varchar`, `char`, `bpchar`, `name` (default 経路)
ビット: **`bit`, `bit varying`** (新規)

### 時刻系(5 種)

`timestamp`, `timestamptz`, `date`, `time`, `timetz`, `interval`

### ネットワーク(4 種)

`inet`, `cidr`, `macaddr`, `macaddr8`

### 半構造(5 種)

`json`, `jsonb`, `xml`, `tsvector`, `tsquery`

### 識別子・特殊(2 種)

`uuid`, money(数値系で重複)

### Geometric(7 種、すべて新規)

**`point`, `line`, `lseg`, `box`, `path`, `polygon`, `circle`**

### コンテナ・拡張(15 種以上)

- 配列(`ARRAY`、要素型問わず)
- 12 種のレンジ/マルチレンジ(`int4range`〜`datemultirange`)
- enum(USER-DEFINED フォールスルー)
- pgvector `vector`(USER-DEFINED フォールスルー)
- **`hstore`**(新規、JSON 化)
- 複合型 composite(USER-DEFINED フォールスルー、PG タプル形式)
- 任意のドメイン型(USER-DEFINED フォールスルー)

**合計: 35+ 種**(Phase 3 の 21 種から 14 種以上を追加)

## Phase 4 残課題

- **PostGIS** `geometry` / `geography` — `ST_AsText` で WKT 化が現実解だが、
  SRID/Z/M の扱いやサイズの問題が残る。実需が出てから着手
- **CI**(GitHub Actions: build + PG smoke + MySQL/TiDB smoke + 21 + 11 = 35+ 型 round-trip)
- **Parquet 出力**(上流 dumpling にも無いので必要性次第)

## 影響範囲

- 変更ファイル: `export/sql.go`(`pgMigrationCast` 内 ~12 行追加)
- 既存挙動への影響: なし(switch case の追加だけ、既存 case は不変)
- テスト: 既存 unit test 通過、e2e で 11 型 + 既存 21 型の混在 round-trip を
  `postgres:17` ↔ `mysql:8.4` で確認
