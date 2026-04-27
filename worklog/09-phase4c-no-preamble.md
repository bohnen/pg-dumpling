# Step 09 — Phase 4c: `--no-preamble` for TiDB Lightning compatibility

## 背景

v0.6.0 (Phase 3) で SQL モードに preamble(SET ブロック)を導入した:

```sql
/*!40101 SET NAMES utf8mb4 */;
/*!40014 SET FOREIGN_KEY_CHECKS=0 */;
/*!40014 SET UNIQUE_CHECKS=0 */;
SET sql_mode='NO_BACKSLASH_ESCAPES';
INSERT INTO `public`.`t` VALUES ...
```

これは `mysql -h … < dump.sql` 直叩きで `'O''Brien'` 形式の quote-doubling が
backslash escape として誤解釈されないために必要。

しかし **TiDB Lightning は裸の SET 文(最後の `SET sql_mode=…`)を弾く**。
Lightning は SQL を実行せず自前パーサで INSERT を抜き出すので、
`NO_BACKSLASH_ESCAPES` を明示しなくても quote-doubling は SQL 標準として
正しく解釈される。preamble 自体が不要。

## 採用した設計

### `--no-preamble` フラグ追加(target-aware default)

```
--no-preamble        Suppress the SET-block at the top of SQL output files.
                     Default: ON for --target=tidb, OFF otherwise.
```

実質的な動作:

| 起動方法 | preamble | 想定ユースケース |
|---|---|---|
| `--target=mysql`(default) | あり | `mysql -h mysql:3306 < dump.sql` |
| `--target=tidb` | **なし** | `tidb-lightning -d /tmp/dump` |
| `--target=pg` | あり | `psql -d db -f dump.sql`(round-trip) |
| `--target=tidb --no-preamble=false` | あり | ad-hoc に `mysql -h tidb:4000 < dump.sql` |
| `--target=mysql --no-preamble` | なし | Lightning に MySQL flavor で食わせる等 |
| `--target=pg --no-preamble` | なし | preamble 不要な PG ロード |

### 適用範囲

preamble は以下の出力ファイルに付く(同じく `--no-preamble` が効く):

- データファイル `<schema>.<table>.NNNNNNNNN.sql`
- `<schema>-schema-create.sql`
- `<schema>.<table>-schema.sql`(pg_dump 出力なので preamble はそもそも上流が
  独自に書くが、pg-dumpling 側で前置していたぶんは抑制される)

CSV ファイルは元々 preamble を持たない(影響なし)。

## 実装

### `export/dialect.go`

```go
// EffectivePreamble returns the SET-block. nil when --no-preamble is set.
func (conf *Config) EffectivePreamble() []string {
    if conf.NoPreamble {
        return nil
    }
    return conf.Dialect().Preamble()
}
```

### `export/config.go`

- `Config.NoPreamble bool`
- フラグ `--no-preamble` 登録
- `ParseFromFlags` の末尾で:
  ```go
  if !flags.Changed(flagNoPreamble) && conf.Target == TargetTiDB {
      conf.NoPreamble = true
  }
  ```
  ユーザーが明示的に渡していない & `--target=tidb` のときだけ強制 ON。

### 既存の preamble 読み出し 3 箇所を `EffectivePreamble()` に置換

- `export/dump.go`: `dumpTableMeta` 内の `tableMeta.specCmts`
- `export/ir.go`: `setTableMetaFromRows`(SQL 直接モード)。シグネチャを
  `(rows, dialect)` から `(rows, conf)` に変更
- `export/writer.go`: `writeMetaToFile`(`-schema-create.sql` / `-schema.sql`)

## Verification(2026-04-27)

`postgres:17` から 5 ケースを dump し、データファイル先頭 2 行を確認。

| 起動 | データファイル先頭 |
|---|---|
| `--target=mysql`(default) | `/*!40101 SET NAMES utf8mb4 */;` |
| `--target=tidb`(default、no-preamble 自動 ON) | `INSERT INTO \`public\`.\`t\` …` |
| `--target=pg` | `SET standard_conforming_strings = on;` |
| `--target=tidb --no-preamble=false` | `/*!40101 SET NAMES utf8mb4 */;`(復活) |
| `--target=mysql --no-preamble` | `INSERT INTO …`(抑制) |

JSON config ログでも `NoPreamble: true/false` が target に応じて切り替わる
ことを確認。

`-schema-create.sql` も同様に preamble の有無が切り替わる:

```
--target=tidb:
    CREATE DATABASE IF NOT EXISTS `public`;

--target=mysql:
    /*!40101 SET NAMES utf8mb4 */;
    /*!40014 SET FOREIGN_KEY_CHECKS=0 */;
    /*!40014 SET UNIQUE_CHECKS=0 */;
    SET sql_mode='NO_BACKSLASH_ESCAPES';
    CREATE DATABASE IF NOT EXISTS `public`;
```

## ユニットテスト

- `TestEffectivePreamble`: 各 target × NoPreamble 組み合わせで preamble の有無
- `TestNoPreambleTargetAwareDefault`: `--target=tidb` の自動 default 切替、
  および `--no-preamble=false` 明示時の上書き

## TiDB Lightning 用の典型ワークフロー(推奨)

```sh
# 1. PG schema を MySQL/TiDB DDL に翻訳して TiDB に流す(LLM 等で)
tidb-lightning -d /tmp/dump --backend tidb \
    -tidb-host tidb -tidb-port 4000 -tidb-user root

# 2. CSV のほうが Lightning と相性が良い場合:
./bin/pg-dumpling -h pg -B mydb -o /tmp/dump --filetype csv \
    --threads 8 --rows 200000

# 3. SQL を Lightning に流したい場合(本ステップで対応):
./bin/pg-dumpling -h pg -B mydb -o /tmp/dump --filetype sql \
    --target tidb --threads 8 --rows 200000
# preamble なし、Lightning が裸の SQL として食える
```

## 影響範囲

- 変更ファイル: `config.go` / `dialect.go` / `dump.go` / `ir.go` / `writer.go`
- 新規: `preamble_test.go`
- 既存挙動への影響: `--target=tidb` のデフォルト出力が変化(preamble なしに)。
  `--target=mysql` / `--target=pg` のデフォルトは不変
- 後方互換: `--no-preamble=false` を明示すれば旧 v0.6.0/v0.8.0 と同じ出力
- 関連: AWS DMS の場合は CDC 用 preamble は影響なし(metadata ファイルのみ)

## マイルストーン

- タグ: `v0.9.0`
- Phase 4 残候補: CI(GitHub Actions)、PostGIS、Parquet
