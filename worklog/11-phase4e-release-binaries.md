# Step 11 — Phase 4e: タグ push で linux/amd64 + linux/arm64 リリースバイナリを自動配布

## 背景・ゴール

Phase 4d で CI は整ったが、ユーザーがバイナリを手に入れるには `git clone &&
make build` か、たった 14 日しか保持されない artifact をダウンロードする
しかなかった。

タグ `v*` を push したタイミングで、

- linux/amd64
- linux/arm64

の 2 アーキバイナリを自動でクロスコンパイルし、GitHub Release のアセットとして
**永続添付**する。

## 設計

### トリガ

`.github/workflows/ci.yml` 既存の `release` ジョブを追加。

```yaml
release:
  if: startsWith(github.ref, 'refs/tags/v')
  needs: [build-and-test, e2e-mysql, e2e-csv, e2e-cdc]
  permissions:
    contents: write           # GitHub Release 作成のため
  strategy:
    fail-fast: false
    matrix:
      goarch: [amd64, arm64]
```

- **`if:` で tag push 時のみ起動**(branch push / PR では走らない)
- **`needs:` で 4 つの既存ジョブが green の時のみ実行**(壊れたバイナリを
  ship しない)
- **matrix で 2 アーキ並列**(壁時計を最小化)

### クロスコンパイル

```yaml
env:
  GOOS: linux
  GOARCH: ${{ matrix.goarch }}
  CGO_ENABLED: "0"
```

`CGO_ENABLED=0` で **static linked**。pg-dumpling のすべての依存(pgx/v5、
br/pkg/storage、prometheus 等)が pure Go なので CGO は不要。出力バイナリは
glibc/musl 問わず動く。

### 成果物

各 matrix run が 3 ファイルを生成:

| ファイル | 用途 |
|---|---|
| `pg-dumpling-v0.X.Y-linux-{amd64,arm64}` | 裸バイナリ。`wget` + `chmod +x` で即実行 |
| `pg-dumpling-v0.X.Y-linux-{amd64,arm64}.tar.gz` | 慣例的なアーカイブ形式 |
| `pg-dumpling-v0.X.Y-linux-{amd64,arm64}.sha256` | チェックサム検証用 |

合計 6 ファイル / リリース。

### Release 作成

`softprops/action-gh-release@v2` を使用:

```yaml
- name: Upload to GitHub Release
  uses: softprops/action-gh-release@v2
  with:
    files: |
      dist/pg-dumpling-${{ github.ref_name }}-linux-${{ matrix.goarch }}
      dist/pg-dumpling-${{ github.ref_name }}-linux-${{ matrix.goarch }}.tar.gz
      dist/pg-dumpling-${{ github.ref_name }}-linux-${{ matrix.goarch }}.sha256
    fail_on_unmatched_files: true
    generate_release_notes: true
```

- **最初の matrix run**: Release を作成 + 3 ファイル添付
- **2 番目の matrix run**: 同じ Release を見つけて 3 ファイル追加
- `generate_release_notes: true`: commit log から changelog を自動生成

### 権限

workflow レベルは `contents: read`(従来)を維持。`release` ジョブのみ
`permissions: contents: write` で上書き。これで CI ジョブが万が一改変されても
write 権限が漏れない設計。

## ローカル検証

push 前に 2 アーキとも cross-compile できることを確認:

```
$ GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/pg-dumpling-arm64 ./cmd/pg-dumpling
$ file /tmp/pg-dumpling-arm64
ELF 64-bit LSB executable, ARM aarch64, statically linked

$ GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/pg-dumpling-amd64 ./cmd/pg-dumpling
$ file /tmp/pg-dumpling-amd64
ELF 64-bit LSB executable, x86-64, statically linked
```

両方 statically linked。

## 影響範囲

- 変更: `.github/workflows/ci.yml`(release ジョブ追加、~50 行)
- 既存 4 ジョブ: 変化なし(tag push 時にも従来どおり走る)
- 新規依存: `softprops/action-gh-release@v2`(GitHub Release CRUD)
- 課金: amd64/arm64 のクロスビルドは ubuntu-latest amd64 ランナー上の `go build`
  でほぼ瞬間的に完了する(~30s)。並列で 2 並走、追加コスト 1 minute 程度

## 運用

リリース手順:

```sh
git tag -a v0.11.0 -m "Phase 4e: release binaries"
git push origin v0.11.0
```

→ CI が走る → 4 ジョブ green になる → release ジョブが起動 → matrix で
amd64/arm64 並列ビルド → GitHub Release `v0.11.0` 作成 → 6 ファイル添付。

ユーザーは:
```sh
wget https://github.com/bohnen/pg-dumpling/releases/download/v0.11.0/pg-dumpling-v0.11.0-linux-amd64
chmod +x pg-dumpling-v0.11.0-linux-amd64
./pg-dumpling-v0.11.0-linux-amd64 --version
```

または:
```sh
curl -L -O https://github.com/bohnen/pg-dumpling/releases/download/v0.11.0/pg-dumpling-v0.11.0-linux-amd64.tar.gz
tar -xzf pg-dumpling-v0.11.0-linux-amd64.tar.gz
```

## マイルストーン

- タグ: `v0.11.0`
- Phase 4 残候補: PostGIS、Parquet、追加プラットフォーム(macOS / Windows)
