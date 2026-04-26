# 02 — MySQL/TiDB strip(連続コミット 02a / 02b / 02c)

- 日付: 2026-04-26
- 担当: tadatoshi.sekiguchi@pingcap.com (with Claude)
- 状態: ✅ build 通過。export/ は 12,500 → 6,067 行に縮小(51% 削減)
- 残タスク(Step 03 で対応): driver 入れ替え、PG 用クエリ書き直し、出力方言、型 map、`pg_dump` shell-out、`bin/pg-dumpling` 改名

## コミットの並び

| commit | tag | 内容 |
|---|---|---|
| `589db59` | `v0.2.0-strip-consistency` | テスト/docs/install.sh/region_results.csv 削除、`consistency.go` 全面書き直し、dump.go の lock/flush 分岐除去 |
| `b7dcdbe` | (なし) | dump.go から TiKV region chunking / TiDB init step を一括削除(1710 → 980 行) |
| `80f595d` | (なし) | sql.go の TiDB/MySQL-only 関数 22 個削除、metadata.go の binlog/GTID 処理を no-op 化、util.go の etcd 連携削除 |

## ゴール

`worklog/01-postgres-rewrite-plan.md` の Step 02 のうち、まず確実に build を通せる範囲で MySQL/TiDB 固有の枝を抜き取る。

PG 専用化は分量が大きいため、本 worklog は **第 1 段**(consistency 周り)。Step 02 の残りは `02b-strip-tidb-paths.md`, `02c-strip-mysql-queries.md` のように分割して進める。

## 今回やったこと

### 削除

- `tests/`(整備していない integration shell 群)
- `docs/`(上流の en/cn ユーザーガイド)
- `install.sh`(minio/mc/snappy/zstd ダウンローダ)
- `export/region_results.csv`(TiKV region テストフィクスチャ)
- 全テストファイル(MySQL/TiDB 動作前提のため。PG 用に書き直す前提で全削除):
  - `block_allow_list_test.go`、`config_test.go`、`consistency_test.go`、`dump_test.go`、`ir_impl_test.go`、`main_test.go`、`metadata_test.go`、`metrics_test.go`、`prepare_test.go`、`sql_test.go`、`sql_type_test.go`、`status_test.go`、`util_for_test.go`、`util_test.go`、`writer_test.go`、`writer_serial_test.go`

### 書き換え

- `export/consistency.go` 全面書き直し
  - `ConsistencyTypeFlush`、`ConsistencyTypeLock` 定数を削除
  - `ConsistencyFlushTableWithReadLock`、`ConsistencyLockDumpingTables` 構造体を削除
  - `ConsistencySnapshot` を **`pg_export_snapshot()` ベースの実装**に置換:
    - メイン接続で `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`
    - `SELECT pg_export_snapshot()` で token を取得して `conf.Snapshot` にセット
    - 外部から `--snapshot <token>` 指定された場合は `SET TRANSACTION SNAPSHOT '<token>'` のみ
  - `ConsistencyTypeAuto` は `ConsistencyTypeSnapshot` にエイリアス(PG では基本 snapshot 一択)
  - `escapeStringLiteral`(SQL 標準クォート二重化)を導入

### dump.go の修正

- `Dump()` から `ConsistencyTypeLock` 早期処理ブロック削除(L161-172)
- `prepareTableListToDump` 呼び出しを単純化(`conf.Consistency != ConsistencyTypeLock` ガード削除、L203-)
- `TransactionalConsistency` ブロックから unlock メッセージ削除(L255-262)
- `getListTableTypeByConf` を 1 行(`return listTableByShowTableStatus`)に簡略化
- `canRebuildConn` を `Snapshot/None/Auto` のみ true 返却に
- `resolveAutoConsistency` を「Auto なら Snapshot に解決」だけに(MySQL/TiDB 分岐と FLUSH probe を全削除)
- 未使用変数 `conn *sql.Conn` を削除

### sql.go

- `snapshotFieldIndex` 定数を `consistency.go` から `sql.go` にローカル移設(`getSnapshot()` がまだ参照しているため)。`getSnapshot()` 自体は次の strip パスで削除予定

## 削除しなかったもの(次パスで対応)

build を通すために、以下は **dead code として一旦残置**:

- `sql.go`
  - `FlushTableWithReadLock`、`LockTables`、`UnlockTables`、`buildLockTablesSQL`、`CheckTiDBEnableTableLock`(consistency 削除で参照ゼロ)
  - `ShowMasterStatus`、`getSnapshot`、`isUnknownSystemVariableErr`、`parseSnapshotToTSO`(TiDB binlog snapshot 用)
  - `GetPdAddrs`、`GetTiDBDDLIDs`、`getTiDBConfig`、`CheckTiDBWithTiKV`、`checkIfSeqExists`(TiDB 専用)
  - `GetPartitionTableIDs`、`GetDBInfo`、`GetRegionInfos`、`GetCharsetAndDefaultCollation`(TiKV 連動メタデータ)
  - `ShowCreatePlacementPolicy`、`ShowCreateSequence`、`ListAllPlacementPolicyNames`、`SelectTiDBRowID`(TiDB DDL/特殊機能)
- `dump.go`
  - `concurrentDumpTiDBTables`、`concurrentDumpTiDBPartitionTables`、`sendConcurrentDumpTiDBTasks`、`selectTiDBTableSample`、`buildTiDBTableSampleQuery`、`selectTiDBRowKeyFields`、`checkTiDBTableRegionPkFields`、`selectTiDBTableRegion`、`selectTiDBPartitionRegion`、`extractTiDBRowIDFromDecodedKey`(TiKV region chunking)
  - `tidbSetPDClientForGC`、`tidbGetSnapshot`、`tidbStartGCSavepointUpdateService`、`detectServerInfo`(初期化 step)
  - `renewSelectTableRegionFuncForLowerTiDB`(古い TiDB region API フォールバック)
- `retry.go`
  - `lockTablesBackoffer`、`newLockTablesBackoffer`(consistency lock 用)
- `metadata.go`
  - GTID/binlog 位置取得処理(MySQL/TiDB 専用)
- `config.go`
  - MySQL 専用フラグ(`--allow-cleartext-passwords`、charset/collation 系)
  - TiDB 専用フラグ(`--tidb-mem-quota-query`、placement-policy 系)

これらは PG 実装(Step 03)に着手する直前 / 並行で削除する。今回は consistency 周りだけ切り出してチェックポイント commit にした。

## ビルド確認

```
$ go build ./...
$ go vet ./...
$ go build -o bin/pg-dumpling ./cmd/dumpling   # 103 MB
```

`bin/pg-dumpling` は build できるが、まだ MySQL ドライバ + MySQL 用 SQL を呼ぶため PG 接続では動かない。Step 03(driver swap + クエリ書き直し)で動作するようになる。

## 次の段(02b 候補)

1. `tidbSetPDClientForGC` / `tidbGetSnapshot` / `tidbStartGCSavepointUpdateService` / `detectServerInfo` を `runSteps` リストから外して関数ごと削除
2. `dump.go` の `concurrentDumpTable` から TiDB region 分岐を削除し、TiDB region 関数群を全削除
3. `dump.go` の `ServerInfo.ServerType == version.ServerTypeTiDB` 分岐を削除
4. `metadata.go` の GTID 取得を削除
5. `sql.go` の `FlushTableWithReadLock`、`UnlockTables`、`LockTables`、`ShowMasterStatus`、`buildLockTablesSQL`、`CheckTiDBEnableTableLock`、`getSnapshot`、`parseSnapshotToTSO`、`GetPdAddrs`、`GetTiDBDDLIDs`、`getTiDBConfig`、`CheckTiDBWithTiKV`、`checkIfSeqExists`、`GetPartitionTableIDs`、`GetDBInfo`、`GetRegionInfos`、`GetCharsetAndDefaultCollation`、`ShowCreatePlacementPolicy`、`ShowCreateSequence`、`ListAllPlacementPolicyNames`、`SelectTiDBRowID` の削除
6. `retry.go` の `lockTablesBackoffer` 削除
7. `config.go` の MySQL/TiDB 専用フラグ削除
8. `br/pkg/version` import の削除

## 次の段(03 — postgres-source)

02 完了後:

1. `go-sql-driver/mysql` を `github.com/jackc/pgx/v5/stdlib` に置換
2. `config.go` の DSN 構築を PG 用に
3. `sql.go` のカタログクエリを PG 用に書き直し(`pg_namespace`、`pg_tables`、`information_schema.columns` 等)
4. `<table>-schema.sql` を `pg_dump --schema-only -t schema.table` の child process 起動で生成
5. `writer_util.go` のバックティック → ダブルクォート、SQL 標準エスケープ
6. `sql_type.go` の型 map を PG 型(`int4`, `text`, `bytea`, `jsonb`, ...)用に
7. `cmd/dumpling/` → `cmd/pg-dumpling/` rename、Makefile・README 更新
