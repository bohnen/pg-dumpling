# Step 10 — Phase 4d: GitHub Actions CI

## ゴール

Phase 1 で「CI 実装は Phase 3 に延期」と決めて以来 deferred になっていた CI を
回収する。Phase 2-4 で手動検証していた e2e シナリオを GitHub Actions の matrix
として常時走らせ、レグレッションを自動で検知する。

## ジョブ構成

`.github/workflows/ci.yml` に 4 ジョブ:

| ジョブ | 内容 | 想定時間 |
|---|---|---|
| **build-and-test** | `go vet` + `make build` + `make test-unit` + バイナリ artifact | ~2 min |
| **e2e-mysql** | `postgres:17` → `--target=mysql` SQL dump → `mysql:8.4` 直叩きロード → 件数 / bytea / JSON / quote-doubling 検証 | ~3 min |
| **e2e-csv** | `postgres:17` → `--filetype=csv --rows 1000` で 5000 行を chunked 出力 → `mysql:8.4` で `LOAD DATA LOCAL INFILE` → 件数検証 | ~3 min |
| **e2e-cdc** | `postgres:17 -c wal_level=logical` で起動 → `--cdc-slot` でレプリケーションスロット作成 → metadata の LSN と `pg_replication_slots.confirmed_flush_lsn` 一致検証 → cleanup-on-failure 検証 | ~3 min |

合計 ~10 min。並列実行されるので壁時計上は ~3 min で全完了する。

## トリガ

```yaml
on:
  push:
    branches: [main]
    tags: ['v*']
  pull_request:
    branches: [main]
```

main への push、PR、リリースタグ(`v*`)のいずれでも走る。

## 詳細

### ビルド環境

- Ubuntu latest(ubuntu-22.04 / 24.04 系)
- Go: `setup-go@v5` で `go-version-file: go.mod` から自動引き当て(現状 1.25.8)
- module cache 有効化(`cache: true`)

### postgresql-client-17

ubuntu-latest 既定の psql は v14 で、postgres:17 サーバの dump に
`pg_dump` を当てると version mismatch で拒否されるため、PGDG apt repo から
17 系を入れている。

```sh
sudo install -d /usr/share/postgresql-common/pgdg
sudo curl --fail -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
  https://www.postgresql.org/media/keys/ACCC4CF8.asc
sudo sh -c 'echo "deb [signed-by=...] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
  > /etc/apt/sources.list.d/pgdg.list'
sudo apt-get update -qq
sudo apt-get install -y --no-install-recommends postgresql-client-17 mysql-client
```

### service container と docker run の使い分け

- `e2e-mysql` / `e2e-csv`: GitHub Actions services (`postgres:17` + `mysql:8.4`)
  を素直に立てる
- `e2e-cdc`: `wal_level=logical` を渡したいので **service container 経由ではなく
  `docker run` で直接起動**(service container の options で postgres コマンドの
  CLI 引数を渡すことは難しい)

### 検証アサーション

#### e2e-mysql で見ているもの

1. 件数(2 行)
2. bytea round-trip: PG 側 `\x48656c6c6f00ff` → MySQL `HEX(b)` で `48656C6C6F00FF` を確認
3. JSON: `JSON_EXTRACT(j, '$.k')` で `"v"`
4. quote-doubling: `O''Brien` → MySQL 側で `O'Brien` を確認

#### e2e-csv で見ているもの

5000 行の chunked CSV(`--rows 1000` で 5 file)が `LOAD DATA LOCAL INFILE` で
全件取り込めること。

#### e2e-cdc で見ているもの

1. `metadata` ファイルに以下が出ること:
   ```
   CDC Slot: ci_smoke_slot
   CDC Plugin: pgoutput
   CDC Consistent Point: 0/<hex>
   CDC Snapshot Name: <token>
   ```
2. `pg_replication_slots.confirmed_flush_lsn` と metadata の Consistent Point が一致
3. ダンプが失敗(出力先 read-only)した場合、デフォルト `--cdc-cleanup-on-failure=true` で
   スロットが drop されている

### artifact

`build-and-test` ジョブが `bin/pg-dumpling` をアップロード(14 日間保持)。
PR から手動で動作確認したい場合に便利。

## 今回スコープ外

- TiDB e2e(`tidb:v8.5.6` を起動する CI ジョブ)。Phase 4 の Verification は手動で
  実施済みで、TiDB のコンテナ起動が遅く CI コストが嵩む割に MySQL 8.4 と差分が
  少ないため当面見送り
- failpoint 系テスト(`make test-unit-failpoint`)。`failpoint-ctl enable` の
  source rewriting が必要で CI 時間を圧迫するため当面 short テストのみ
- Linux/amd64 以外のクロスビルド(必要になったら追加)

## 影響範囲

- 新規: `.github/workflows/ci.yml`(~250 行)、`worklog/10-...`
- 既存挙動への影響: なし(リポジトリ側だけの追加)
- 課金: GitHub Actions のパブリックリポジトリは無料枠内

## マイルストーン

- タグ: `v0.10.0`(予定)
- Phase 4 残候補: PostGIS、Parquet
