# 00 — ベースライン作成: TiDB monorepo から dumpling を切り出す

- 日付: 2026-04-26
- 担当: tadatoshi.sekiguchi@pingcap.com (with Claude)
- 結果: ✅ ビルド可能な独立リポジトリとしてベースライン確立

## ゴール

`~/Work/tidb/dumpling` にある dumpling のソースを、TiDB monorepo から物理的に切り出して、`/Users/bohnen/Project/pg-dumpling/` 単体で `go build` が通る形にする。**機能変更はゼロ**。Postgres 対応は次フェーズ以降。

## 設計判断(ユーザー確認済)

| 項目 | 採用 | 理由 |
|---|---|---|
| Module path | `github.com/tadapin/pg-dumpling` | 個人 GitHub への push を想定。あとで変える必要なし |
| TiDB 依存解決 | ローカル `replace` で `~/Work/tidb` を指す | Phase 0 では確実性優先。public pseudo-version pin は go mod download が数百 MB になる |
| `tests/`(integration shell) | コピーするが実行はしない | 後で TiDB/MinIO を立てて回す可能性のために残す |

## やったこと

### 1. ファイル配置

`cp` で以下を `pg-dumpling/` 直下に複製(`BUILD.bazel`/`OWNERS`/`revive.toml` は除外):

```
cli/versions.go
cmd/dumpling/main.go
context/context.go
log/{log.go,log_test.go}
export/*.go              # 39 ファイル
export/region_results.csv # 82 KB のテストフィクスチャ
tests/                   # 上流 integration tests 一式
docs/                    # 上流ドキュメント
README.md
install.sh
LICENSE                  # ~/Work/tidb/LICENSE から
```

### 2. import path 書き換え

```sh
find . -name '*.go' -exec sed -i '' \
  's|github.com/pingcap/tidb/dumpling/|github.com/tadapin/pg-dumpling/|g' {} +
```

29 ファイルを一括置換。`failpoint.Enable("github.com/.../export/FailToCloseDataFile", ...)` のような文字列識別子も同じパスを使っているので一緒に置換される(これが正しい挙動)。

`github.com/pingcap/tidb/{br,pkg}/...` は **触らない**。go.mod の replace で解決する。

### 3. `go.mod` 作成

```
module github.com/tadapin/pg-dumpling

go 1.25.8

require (
    github.com/pingcap/tidb v0.0.0-00010101000000-000000000000
)

replace (
    github.com/apache/arrow-go/v18 => github.com/joechenrh/arrow-go/v18 v18.0.0-20250911101656-62c34c9a3b82
    github.com/go-ldap/ldap/v3 => github.com/YangKeao/ldap/v3 v3.4.5-0.20230421065457-369a3bab1117
    github.com/pingcap/tidb => /Users/bohnen/Work/tidb
    github.com/pingcap/tidb/pkg/parser => /Users/bohnen/Work/tidb/pkg/parser
    sourcegraph.com/sourcegraph/appdash => github.com/sourcegraph/appdash v0.0.0-20190731080439-ebfcffb1b5c0
    sourcegraph.com/sourcegraph/appdash-data => github.com/sourcegraph/appdash-data v0.0.0-20151005221446-73f23eafcf67
)
```

`Makefile` と `.gitignore` も新規作成。Makefile の build target は LDFLAGS で `cli.{ReleaseVersion,BuildTimestamp,GitHash,GitBranch,GoVersion}` を埋める(上流と同等)。

### 4. `go mod tidy` → `go build ./...` → `bin/dumpling --help` 検証

成功。バイナリは 103 MB(上流とほぼ同等のサイズ)。`--help` でフラグ約 50 個が表示され、parser/storage 系すべてリンク完了。

## ハマったポイント(将来のために記録)

### (a) Go toolchain は patch 番号必須

最初 `go 1.25` と書いたら、`GOTOOLCHAIN=auto` が `go1.25` という非実在バージョンを取りに行って失敗:

```
go: download go1.25 for darwin/arm64: toolchain not available
```

→ **TiDB 本体と同じ `go 1.25.8` に揃える**ことで `go1.25.8` toolchain を自動取得して解決。

### (b) `pkg/parser` は別の Go モジュール

dumpling は `github.com/pingcap/tidb/pkg/parser` を import しているが、これは monorepo 内に独立した `go.mod` を持つサブモジュール:

```
/Users/bohnen/Work/tidb/go.mod
/Users/bohnen/Work/tidb/pkg/parser/go.mod
```

→ TiDB 本体への replace だけでは届かないため、`pkg/parser` 用に **個別の replace 行**が必要:

```
github.com/pingcap/tidb/pkg/parser => /Users/bohnen/Work/tidb/pkg/parser
```

### (c) 上流 go.mod の `replace` は下流に伝播しない

これは Go modules の仕様。TiDB の go.mod には 4 件の replace(arrow-go, ldap, appdash 系)があり、これらが効かないと `go mod tidy` 中の依存解決が失敗する。

→ **TiDB の `replace` ブロックを我々の go.mod に丸ごとコピー**して解決。将来 TiDB が replace を増減したら、こちらも同期する必要がある(`grep -A 10 '^replace (' ~/Work/tidb/go.mod` で確認)。

### (d) failpoint テストは事前書き換えが必要

`go test ./export/...` で 2 件失敗:

```
--- FAIL: TestWriteTableMeta — An error is expected but got nil.
--- FAIL: TestWriteTableData — An error is expected but got nil.
```

これらは `failpoint.Enable("github.com/tadapin/pg-dumpling/export/FailToCloseDataFile", "return(true)")` で意図的にエラー注入を有効化するテスト。素の状態だと `failpoint` ライブラリは `_curpkg_(...)` 呼び出しが noop のまま動くため、注入が効かずエラーが返らない。

→ TiDB 上流と同じく `failpoint-ctl enable` でソースを書き換えてから test を回す必要がある。Makefile に `test-unit-failpoint` ターゲットを別途用意:

```sh
go install github.com/pingcap/failpoint/failpoint-ctl@latest
make test-unit-failpoint
```

これで全部 pass するはず(本セッションでは `failpoint-ctl` 未インストールのため未検証)。

## 検証結果

| 項目 | 結果 |
|---|---|
| `go mod tidy` | ✅ |
| `go build ./...` | ✅ 無エラー |
| `go vet ./...` | ✅(make build と同時に通る) |
| `bin/dumpling --version` | ✅ `Release version: dev` / `Go version: go1.25.8` |
| `bin/dumpling --help` | ✅ 全フラグ約 50 個表示 |
| `go test ./log/...` | ✅ 1 パッケージ pass |
| `go test ./context/...`, `./cli/...` | ✅(テストファイル無し) |
| `go test ./export/...` | ⚠️ failpoint 依存の 2 件のみ失敗、それ以外 pass |
| `tests/`(integration) | ⏸ Phase 0 では未稼働 |

## 次のステップ(Phase 1 候補)

1. **lint / format 整備**: `revive.toml` 復活、`gofumpt` か `goimports`、`make lint` 整備
2. **CI**: GitHub Actions で `make build && make test-unit` を回す。TiDB ローカル checkout 依存をどうするかが論点(`pingcap/tidb` を git clone する CI ステップ?)
3. **TiDB 内部依存の段階剥がし**:
   - 軽い順に: `pkg/errno` → `pkg/util/{filter,dbutil,promutil}` → `pkg/meta/model` → `br/pkg/version` → `br/pkg/utils` をリポジトリ内にコピー
   - 重い順は後回し: `pkg/parser` 系、`pkg/tablecodec`、`pkg/store/helper` は Postgres 化のときに削除予定
4. **Postgres 接続のスケルトン**: `export/conn.go` / `export/sql.go` で MySQL 前提の場所を抽象化、`pq` ドライバを使った PG 用パスを追加(別ブランチで実験)

## 参考: 残っている TiDB 内部 import の一覧

`grep -rh '^\s*"github.com/pingcap/tidb' --include='*.go' . | sort -u` で確認可能。Phase 1 で 1 件ずつ片付ける作業対象。
