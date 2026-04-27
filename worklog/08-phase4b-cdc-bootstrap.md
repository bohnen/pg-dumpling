# Step 08 — Phase 4b: AWS DMS / 論理デコーディング向け CDC ブートストラップ

## 背景・ゴール

Phase 3 までで「PG → MySQL/TiDB の初期ダンプ」は SQL/CSV 両ルートで動くようになった。
現実の移行運用では、初期ダンプ完了後に **CDC で差分を取り込み続ける**必要がある
(AWS DMS、Debezium、`pg_recvlogical` など)。

CDC consumer に **取りこぼし・重複なく** 引き渡すには、ダンプの MVCC スナップショット
の取得点と、CDC の起点 LSN が**アトミックに一致**していなければならない。これは PG
の replication プロトコル `CREATE_REPLICATION_SLOT … EXPORT_SNAPSHOT` でしか実現できない。

Phase 4b ではこれを pg-dumpling に組み込み、AWS DMS(test_decoding / pglogical_output /
pgoutput)に渡せる形で `metadata` ファイルにスロット情報を出力する。

## 設計判断(ユーザー承認: 2026-04-27)

| 項目 | 決定 |
|---|---|
| DMS の起動タイミング | 手動。pg-dumpling は metadata に slot 名 + LSN を残すだけ |
| metadata フォーマット | 既存 dumpling 形式(`Key: Value` テキスト)に行を追加するだけ |
| `--cdc-cleanup-on-failure` | **デフォルト ON**。失敗したダンプが slot を残して WAL を蓄積する事故を防ぐ |
| pglogical サポート | プラグイン名 `pglogical_output` で同じ実装に乗る。pglogical extension の node 設定はユーザー責任 |
| デフォルトプラグイン | `pgoutput`(PG 標準、何もインストール不要、AWS DMS でも対応) |

## 追加した CLI フラグ

```
--cdc-slot <name>                  論理レプリケーションスロット名
                                   (空: CDC 機能オフ)
--cdc-plugin {pgoutput, test_decoding, pglogical_output}
                                   出力プラグイン (default pgoutput)
--cdc-cleanup-on-failure=true|false
                                   ダンプ失敗時にスロットを drop するか (default true)
```

slot/plugin 名は replication プロトコルの制約により `[a-z0-9_]{1,63}` のみ許可
(quoted identifier がプロトコル上使えないため、CLI 入力時点でバリデート)。

## 実装ファイル

### `export/cdc.go`(新規)

- `cdcSlotInfo` 構造体: SlotName / ConsistentPoint / SnapshotName / OutputPlugin
- `ValidateCDCName(flag, name)`: `[a-z0-9_]{1,63}` バリデーション
- `openReplicationConn(ctx, dsn)`: `pgconn.ConnectConfig` で `RuntimeParams["replication"] = "database"` を立てた接続を開く
- `createCDCSlot(ctx, dsn, name, plugin) (*pgconn.PgConn, *cdcSlotInfo, error)`:
  - replication 接続を開く
  - `CREATE_REPLICATION_SLOT <name> LOGICAL <plugin> EXPORT_SNAPSHOT` を発行
  - 戻り値の 4 列(slot_name / consistent_point / snapshot_name / output_plugin)をパース
  - 接続は **開いたまま返す**(snapshot は接続生存中のみ有効)
- `dropCDCSlot(ctx, dsn, name)`: 失敗時クリーンアップ。新規 replication 接続から
  `DROP_REPLICATION_SLOT <name>` を発行(active な接続が残ってると drop 不可なので
  必ず新規接続を使う)

### `export/consistency.go`

`ConsistencySnapshot` に以下を追加:

- フィールド: `cdcConn *pgconn.PgConn`、`cdcSlot *cdcSlotInfo`
- `Setup`: `--cdc-slot` 指定時は `pg_export_snapshot()` の代わりに `createCDCSlot` を呼ぶ。
  返ってきた `snapshot_name` を既存の `SET TRANSACTION SNAPSHOT` パイプラインに乗せる
- `TearDown`: cdcConn を close(slot 自体は残る → DMS が拾える)
- `OnFailure`(新規): `--cdc-cleanup-on-failure=true` のとき新規接続から DROP_REPLICATION_SLOT
- `ConsistencyController` interface に `OnFailure(ctx) error` を追加。`ConsistencyNone` は no-op

### `export/dump.go`

deferred ブロックを更新:

```go
defer func() {
    if tdErr := conCtrl.TearDown(tctx); tdErr != nil { ... warn ... }
    if err != nil {
        if cleanErr := conCtrl.OnFailure(tctx); cleanErr != nil { ... warn ... }
    }
}()
```

`err` は名前付き戻り値。Setup 後に `m.cdcSlot = conf.cdcSlotInfo` で metadata にスロット
情報を伝搬。

### `export/metadata.go`

`globalMetadata` に `cdcSlot *cdcSlotInfo` 追加。`recordGlobalMetaData` で以下を出力:

```
CDC Slot: <slot_name>
CDC Plugin: <plugin>
CDC Consistent Point: <LSN>
CDC Snapshot Name: <snapshot_name>
```

### `export/config.go`

- `Config.CDCSlot` / `CDCPlugin` / `CDCCleanupOnFailure` / `cdcSlotInfo` を追加
- DefaultConfig: `CDCPlugin: "pgoutput"`, `CDCCleanupOnFailure: true`
- DefineFlags / ParseFromFlags に対応

## Verification(2026-04-27)

`postgres:17` を `wal_level=logical` で起動し、4 ケースを実機検証:

### 1. 成功ケース(pgoutput)

```sh
./bin/pg-dumpling -h pg -B cdcdemo -o /tmp/dump-cdc \
    --filetype sql --target mysql \
    --cdc-slot dms_cdcdemo --cdc-plugin pgoutput
```

ログ:
```
created CDC replication slot
  slot=dms_cdcdemo plugin=pgoutput
  consistent_point=0/193BEE0
  snapshot_name=00000071-00000002-1
```

`metadata`:
```
Started dump at: 2026-04-27 10:18:53
CDC Slot: dms_cdcdemo
CDC Plugin: pgoutput
CDC Consistent Point: 0/193BEE0
CDC Snapshot Name: 00000071-00000002-1
Finished dump at: 2026-04-27 10:18:54
```

`pg_replication_slots`:
```
 slot_name  | plugin   | slot_type | database | active | restart_lsn | confirmed_flush_lsn
-------------+----------+-----------+----------+--------+-------------+---------------------
 dms_cdcdemo | pgoutput | logical   | cdcdemo  | f      | 0/193BEA8   | 0/193BEE0
```

→ slot が残り、`confirmed_flush_lsn` がダンプの consistent_point と一致。
DMS source endpoint で `slotName=dms_cdcdemo, pluginName=pgoutput` を指定すれば
ここから CDC 再生できる。

### 2. test_decoding プラグイン

`--cdc-plugin test_decoding` でも同じくスロット作成成功、metadata に
`CDC Plugin: test_decoding` が記録される。プラグインの違いは PG 側のメッセージ
形式のみで、pg-dumpling 側のロジックは共通。

### 3. 失敗時 cleanup-on-failure=true(デフォルト)

出力ディレクトリを read-only にしてダンプを強制失敗させた:

```sh
chmod 555 /tmp/dump-cdc-fail2
./bin/pg-dumpling ... --cdc-slot dms_cleanup_test
```

ログ:
```
created CDC replication slot ... slot=dms_cleanup_test
permission denied
dump failed
```

ダンプ失敗後の `pg_replication_slots`:
```
 slot_name
-----------
(0 rows)
```

→ スロットがちゃんと drop されている。WAL 保持リスクなし。

### 4. 失敗時 cleanup-on-failure=false

`--cdc-cleanup-on-failure=false` を付けて同じ失敗シナリオ:

```
 dms_keep_test | 0/193CB70
```

→ スロット保持。手動再開や調査用途。

## DMS との接続例

### Test Decoding / pgoutput ルート(推奨)

PG セットアップ:
```sh
# wal_level=logical 必須(RDS の場合は parameter group で設定)
ALTER SYSTEM SET wal_level = logical;
SELECT pg_reload_conf();   -- 反映には再起動が必要なことに注意
```

ダンプ:
```sh
./bin/pg-dumpling -h pg -B mydb -o /tmp/dump \
    --filetype sql --target mysql \
    --cdc-slot dms_mydb --cdc-plugin pgoutput
```

`metadata` の `CDC Slot` / `CDC Consistent Point` を控える。

DMS タスク作成:
- Source endpoint extra connection attributes:
  - `pluginName=pgoutput`
  - `slotName=dms_mydb`
- Migration type: **CDC only** (DMS の full load はスキップ)
- Start position: `metadata` の Consistent Point(オプション。スロットの
  `confirmed_flush_lsn` から自動継続するなら省略可)

### pglogical ルート

事前に PG 側で:
```sql
CREATE EXTENSION pglogical;
SELECT pglogical.create_node(node_name := 'mydb_provider',
                             dsn := 'host=pg dbname=mydb');
SELECT pglogical.create_replication_set(set_name := 'all_tables');
SELECT pglogical.replication_set_add_all_tables('all_tables', ARRAY['public']);
```

ダンプ:
```sh
./bin/pg-dumpling ... --cdc-slot dms_mydb --cdc-plugin pglogical_output
```

DMS は `pluginName=pglogical, slotName=dms_mydb` で接続。

**注**: pglogical extension は AWS DMS 環境(RDS / Aurora / セルフホスト)で
利用可否や設定方法が違う。本ツールはスロット作成だけ担当し、node /
replication_set のセットアップはユーザー責任。

## 制約と運用上の注意

### 1. `wal_level = logical` が必要

これが標準でないと `CREATE_REPLICATION_SLOT` が失敗する。

### 2. RDS / Aurora PostgreSQL 固有の設定

- `rds.logical_replication = 1` パラメータグループで設定
- `replication` ロールが必要(`rds_replication` を grant)
- AWS DMS の master ユーザーで pg-dumpling を実行するか、最低限
  `LOGIN REPLICATION` 権限を持つロールを使う

### 3. スロットを残したまま dump 完了 → CDC 接続までの時間制限

スロットは未消費 WAL を保持し続けるため、dump 完了後に DMS / consumer の接続が
遅れると WAL が溜まりディスク逼迫の可能性がある。**dump 終了次第すぐ DMS タスクを
起動**すること。

### 4. 同名スロットの再利用不可

`CREATE_REPLICATION_SLOT` は同名のスロットがあると失敗する。再実行する場合は
事前に `SELECT pg_drop_replication_slot('xxx')` するか、別名にすること。

### 5. snapshot は replication 接続生存中のみ有効

`SET TRANSACTION SNAPSHOT` を使い終わるまで `cdcConn` は close できない。
TearDown で初めて close する設計。

## バリデーション(`ValidateCDCName`)

slot / plugin 名は `[a-z0-9_]{1,63}` に厳格に制限。replication プロトコルでは
**quoted identifier(`"foo"`)を受け付けない**ため、SQL injection や予期せぬ識別子は
ここで弾く。

```
ok:   mycdc, slot_1, pgoutput, pglogical_output, test_decoding
ng:   MyCDC (uppercase), my-cdc (hyphen), my.cdc (dot),
      slot;DROP TABLE x (semicolon), 64+ chars
```

unit test: `export/cdc_test.go::TestValidateCDCName`

## 影響範囲

- 新規: `export/cdc.go`(~140 行)、`export/cdc_test.go`(~50 行)
- 改修: `export/consistency.go`(~70 行追加)、`export/metadata.go`(~15 行)、
  `export/dump.go`(~5 行)、`export/config.go`(~30 行)
- 既存挙動への影響: なし(`--cdc-slot` 未指定時は何も変わらない)

## マイルストーン

- タグ: `v0.8.0`(予定)
- Phase 4 残候補: CI(GitHub Actions)、PostGIS、Parquet
