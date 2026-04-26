# pg-dumpling — Claude 向けプロジェクトメモ

## このプロジェクトは何か

`dumpling`(MySQL/TiDB 用ダンプツール)を TiDB monorepo (`github.com/pingcap/tidb`) から切り出した独立リポジトリ。最終目標は **Postgres をソースとして対応させた dumpling**。今は **Phase 0(ベースライン)** が完了した状態 — 機能は上流と同じで、`pg-dumpling/` 単体でビルドが通るところまで。

- Module path: `github.com/tadapin/pg-dumpling`
- Go: **1.25.8**(`go.mod` の `go` ディレクティブで指定。`GOTOOLCHAIN=auto` で自動取得される)
- 上流ベース: `~/Work/tidb` のスナップショット(2026-04-26 時点、TiDB commit `ae18096e02378`)

## ビルド & テスト

```sh
make build                  # bin/dumpling を生成
make test-unit              # log/, export/ の通常テスト
make test-unit-failpoint    # failpoint 書き換え後にフルテスト
make tidy                   # go mod tidy
make vet                    # go vet ./...
make clean                  # bin/ を削除
```

`bin/dumpling --version` / `--help` が動けば baseline 健全性 OK。

## 重要な制約

### TiDB ローカルチェックアウトに依存している

`go.mod` に以下の `replace` 群があり、`/Users/bohnen/Work/tidb` を消すと build できない:

```
github.com/pingcap/tidb        => /Users/bohnen/Work/tidb
github.com/pingcap/tidb/pkg/parser => /Users/bohnen/Work/tidb/pkg/parser
github.com/apache/arrow-go/v18 => github.com/joechenrh/arrow-go/v18 ...
github.com/go-ldap/ldap/v3     => github.com/YangKeao/ldap/v3 ...
sourcegraph.com/sourcegraph/appdash      => github.com/sourcegraph/appdash ...
sourcegraph.com/sourcegraph/appdash-data => github.com/sourcegraph/appdash-data ...
```

後ろの 4 件は **TiDB 上流の `go.mod` にある replace を転記しているもの**。Go の replace は下流(我々)に伝播しないため必須。**下手に削らないこと。**

### `pkg/parser` は TiDB monorepo 内の別 Go モジュール

TiDB 本体とは別の go.mod を持つので、`replace github.com/pingcap/tidb/pkg/parser => /Users/bohnen/Work/tidb/pkg/parser` を別行で指定する必要がある(本体側の replace では届かない)。

### Go バージョンは patch まで指定

`go 1.25` では toolchain ダウンロードが `go1.25` という存在しないバージョンを取りに行って失敗する。**必ず `go 1.25.8` のように patch 番号まで書く**(TiDB 本体と同じ値に揃える)。

### failpoint の扱い

`export/writer_test.go` の `TestWriteTableMeta` / `TestWriteTableData` は `github.com/pingcap/failpoint` 経由でファイルクローズ失敗を注入するテスト。これらは事前に `failpoint-ctl enable` でソース書き換えを行わないと "An error is expected but got nil" で失敗する。`make test-unit-failpoint` がそれを面倒見る。

## ディレクトリ構成

```
cli/             バージョン情報埋め込み(LDFLAGS で書き換え)
cmd/dumpling/    main(import 5 行のみ)
context/         logger を埋め込んだ context.Context wrapper
export/          ★ 本体。dump engine の全機能(39 ファイル)
log/             zap ラッパ
tests/           上流の shell-driven integration tests(現状未稼働)
docs/            上流ユーザードキュメント(en/cn)
worklog/         本リポジトリでの作業履歴(連番)
```

`export/` の中身グループ:
- 設定: `config.go`, `block_allow_list.go`
- dump 駆動: `dump.go`(本体ループ)、`task.go`、`prepare.go`
- SQL 生成: `sql.go`, `sql_type.go`
- Writer / IR: `writer.go`, `writer_util.go`, `ir.go`, `ir_impl.go`
- メタデータ: `metadata.go`, `consistency.go`, `status.go`
- 横断: `metrics.go`, `http_handler.go`, `retry.go`, `conn.go`, `util.go`

## Phase 1 で剥がしていく予定の TiDB 内部依存

| 依存 | 重さ | Postgres 化での扱い |
|---|---|---|
| `pkg/parser`, `pkg/parser/{ast,format,model}` | 重 | **削除予定**(MySQL DDL の AST 解析に使われている。Postgres では不要) |
| `pkg/tablecodec`, `pkg/store/helper` | 重 | **削除予定**(TiKV region split 用、Postgres には無関係) |
| `pkg/errno` | 軽 | **置換予定**(MySQL エラーコード → Postgres SQLSTATE) |
| `br/pkg/storage` | 重 | 維持。S3/GCS/Azure 抽象は欲しい |
| `br/pkg/{utils,version,summary}` | 中軽 | 必要部分のみコピーして取り込み |
| `pkg/meta/model` | 中 | 自前型に置換 or コピー |
| `pkg/util/*`(`table-filter`, `dbutil`, `codec`, `promutil`, `filter`) | 軽 | 必要分コピー |
| `pkg/config`, `pkg/infoschema/context` | 軽 | コピー or 削除可 |

## 作業のお作法

- **作業ログは `worklog/<連番>-<短い説明>.md`** に時系列で残す。後から差分の意図を追えるように。
- **import の書き換え**は `find . -name '*.go' -exec sed -i '' 's|<旧>|<新>|g' {} +` 一括が無難。failpoint の文字列識別子(`failpoint.Enable("github.com/.../export/Foo")`)も同じパスで書かれているので一緒に置換される — これは正しい挙動。
- **bazel `BUILD.bazel` は本リポジトリでは持たない**。`go build` 一本。
- **OWNERS / revive.toml は持たない**(必要なら lint 設定は別途検討)。

## ライセンス

Apache 2.0、上流 `pingcap/tidb` から継承。`LICENSE` ファイル参照。
