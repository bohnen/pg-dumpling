# Step 05 — Phase 3a: replace ~/Work/tidb の撤去 (public commit pin 化)

## 背景

v0.4.0 (Phase 2) 完了時点で、`go.mod` には依然として:

```
replace github.com/pingcap/tidb         => /Users/bohnen/Work/tidb
replace github.com/pingcap/tidb/pkg/parser => /Users/bohnen/Work/tidb/pkg/parser
```

が残っており、ビルドにはローカルチェックアウト
`/Users/bohnen/Work/tidb`(v8.5.6 タグ、commit `ae18096e0237...`)が必須だった。
実際に import している TiDB 内部パッケージは **`br/pkg/storage` 1 個だけ**で、
これはクラウドバックエンド対応(S3/GCS/Azure 抽象)のため意図的に残している。

## 採用した解決策

ローカルチェックアウトを **public commit pin**(`proxy.golang.org` 経由で
解決される pseudo-version)に置換。

| パッケージ | Before | After |
|---|---|---|
| `github.com/pingcap/tidb` | `replace => /Users/bohnen/Work/tidb` | `v1.1.0-beta.0.20260413061245-ae18096e0237` |
| `github.com/pingcap/tidb/pkg/parser` | `replace => /Users/bohnen/Work/tidb/pkg/parser` | `v0.0.0-20260331085336-4e0b702f38a8`(indirect) |

両 commit は v8.5.6 タグ時点の public github.com/pingcap/tidb と
github.com/pingcap/tidb/pkg/parser サブツリー上に存在する。

(参考: TiDB は `v8.5.6` のような Git タグは打っているが、Go module proxy には
v2 系までしか incompatible 公開されていないため、commit ベースの pseudo-version で
固定するのが現実的)

## 検討した代替案

| 案 | 採否 | 理由 |
|---|---|---|
| `gocloud.dev/blob` で `br/pkg/storage` を置換 | ❌ | クラウド固有フラグ群(`--s3.role-arn`、`--s3.sse-kms-key-id`、`--gcs.predefined-acl`、`--azblob.encryption-key-...`)を表現するのに ~500-800 行の adapter を自作する必要があり、上流改善追従コストも継続的に発生する |
| `internal/storage/` に丸ごと vendor | ❌ | transitive 依存(`br/pkg/{errors,logutil,utils,utils/iter}`、`pkg/util/{intest,logutil,prefetch}`、`pkg/sessionctx/variable`、`pkg/lightning/log`)が多すぎて単純コピーでは独立できない |
| `v8.5.6+incompatible` を直接 require | ❌ | `proxy.golang.org/github.com/pingcap/tidb/@v/list` に v8 系の incompatible が公開されていない(最新 incompatible は v2.x 系)ため使えない |
| **public commit pin (pseudo-version)** | ✅ | `go get github.com/pingcap/tidb@<commit>` で proxy が自動解決、コードも UX も完全に互換、依存重量も Phase 2 と同じ |

## 適用手順(再現可能)

```sh
# 1) ローカル replace を撤去
sed -i '' '/github.com\/pingcap\/tidb => \/Users\/bohnen\/Work\/tidb/d' go.mod
sed -i '' '/github.com\/pingcap\/tidb\/pkg\/parser => \/Users\/bohnen\/Work\/tidb\/pkg\/parser/d' go.mod

# 2) 仮の pseudo-version を入れる (placeholder の v0.0.0-00010101 を解消するため)
#    既知の v8.5.6 commit のタイムスタンプ (2026-04-13 06:12:45 UTC) +
#    short hash (12 文字) でフォーマット
#    -> v0.0.0-20260413061245-ae18096e0237

# 3) 公式 proxy から実際の pseudo-version を解決させる
go get github.com/pingcap/tidb@ae18096e023780bb56bfce33698abec0d4640d0a
# => v1.1.0-beta.0.20260413061245-ae18096e0237

# 4) go mod tidy で indirect / sum をクリーンアップ
go mod tidy

# 5) ビルド・テスト確認
make build
make test-unit
./bin/pg-dumpling --version
```

## Verification

```
$ make build
go build -ldflags '...' -o bin/pg-dumpling ./cmd/pg-dumpling
(no errors)

$ ./bin/pg-dumpling --version
Release version: dev
Git commit hash: f3af2a73...
Build timestamp: 2026-04-26 ...
Go version:      go1.25.8

$ make test-unit
=== RUN   TestInitLogNoPermission
--- PASS: TestInitLogNoPermission (0.00s)
PASS
ok  github.com/tadapin/pg-dumpling/log  0.343s
?   github.com/tadapin/pg-dumpling/export  [no test files]
```

end-to-end の PG → TiDB CSV 移行は v0.4.0 時点で `tidb:v8.5.6` に対し検証済み。
本ステップでは `br/pkg/storage` 等のコードは 1 行も変更していないため、
バイナリ動作は v0.4.0 と完全に同等。

## go.mod の最終形(抜粋)

```
require (
    ...
    github.com/pingcap/tidb v1.1.0-beta.0.20260413061245-ae18096e0237
    ...
)

require (
    ...
    github.com/pingcap/tidb/pkg/parser v0.0.0-20260331085336-4e0b702f38a8 // indirect
    ...
)

replace (
    github.com/apache/arrow-go/v18 => github.com/joechenrh/arrow-go/v18 v18.0.0-20250911101656-62c34c9a3b82
    github.com/go-ldap/ldap/v3 => github.com/YangKeao/ldap/v3 v3.4.5-0.20230421065457-369a3bab1117
    sourcegraph.com/sourcegraph/appdash => github.com/sourcegraph/appdash v0.0.0-20190731080439-ebfcffb1b5c0
    sourcegraph.com/sourcegraph/appdash-data => github.com/sourcegraph/appdash-data v0.0.0-20151005221446-73f23eafcf67
)
```

`replace` には TiDB 上流由来の非 TiDB 系のみが残る。これらは TiDB が
ローカル fork を使っているため、downstream consumer も同じ replace を
書く必要がある(replace は consumer に継承されない)。

## バンプ運用

TiDB のバージョンを上げたいときは:

```sh
# 例: 別の v8.5.x commit を pin する
go get github.com/pingcap/tidb@<full-or-short-commit>
go mod tidy
make build && make test-unit
```

`pkg/parser` のサブツリーが移動した場合は indirect 行も同様に
`go get github.com/pingcap/tidb/pkg/parser@<commit>` で解決できる。

## マイルストーン

- タグ: `v0.5.0`
- コミットメッセージ:
  ```
  Step 05: replace ~/Work/tidb 撤去 (public commit pin)

  TiDB を v8.5.6 commit ae18096e (= proxy.golang.org の
  v1.1.0-beta.0.20260413061245-ae18096e0237) に固定。
  ローカルチェックアウト依存を完全解消。コード変更なし、
  go.mod / go.sum / docs の更新のみ。
  ```

## Phase 3 残候補

- CI(GitHub Actions: build + PG smoke + TiDB smoke)
- PostGIS / hstore / tsvector など特殊型対応
- Parquet 出力(優先度低、上流 dumpling にも無い)
