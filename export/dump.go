// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	// register pgx as the database/sql driver
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	pclog "github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/summary"
	"github.com/tadapin/pg-dumpling/cli"
	tcontext "github.com/tadapin/pg-dumpling/context"
	"github.com/tadapin/pg-dumpling/log"
	gatomic "go.uber.org/atomic"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)


// Dumper is the dump progress structure
type Dumper struct {
	tctx      *tcontext.Context
	cancelCtx context.CancelFunc
	conf      *Config
	metrics   *metrics

	extStore storage.ExternalStorage
	dbHandle *sql.DB

	totalTables                   int64
	charsetAndDefaultCollationMap map[string]string

	speedRecorder *SpeedRecorder
}

// NewDumper returns a new Dumper
func NewDumper(ctx context.Context, conf *Config) (*Dumper, error) {
	failpoint.Inject("setExtStorage", func(val failpoint.Value) {
		path := val.(string)
		b, err := storage.ParseBackend(path, nil)
		if err != nil {
			panic(err)
		}
		s, err := storage.New(context.Background(), b, &storage.ExternalStorageOptions{})
		if err != nil {
			panic(err)
		}
		conf.ExtStorage = s
	})

	tctx, cancelFn := tcontext.Background().WithContext(ctx).WithCancel()
	d := &Dumper{
		tctx:          tctx,
		conf:          conf,
		cancelCtx:     cancelFn,
		speedRecorder: NewSpeedRecorder(),
	}

	var err error

	d.metrics = newMetrics(conf.PromFactory, conf.Labels)
	d.metrics.registerTo(conf.PromRegistry)
	defer func() {
		if err != nil {
			d.metrics.unregisterFrom(conf.PromRegistry)
		}
	}()

	err = adjustConfig(conf,
		buildTLSConfig,
		validateSpecifiedSQL,
		adjustFileFormat)
	if err != nil {
		return nil, err
	}
	failpoint.Inject("SetIOTotalBytes", func(_ failpoint.Value) {
		d.conf.IOTotalBytes = gatomic.NewUint64(0)
		d.conf.Net = uuid.New().String()
		go func() {
			for {
				time.Sleep(10 * time.Millisecond)
				d.tctx.L().Logger.Info("IOTotalBytes", zap.Uint64("IOTotalBytes", d.conf.IOTotalBytes.Load()))
			}
		}()
	})

	err = runSteps(d,
		initLogger,
		createExternalStore,
		startHTTPService,
		openSQLDB,
		resolveAutoConsistency,
		validateResolveAutoConsistency,
		setSessionParam)
	return d, err
}

// Dump dumps table from database
// nolint: gocyclo
func (d *Dumper) Dump() (dumpErr error) {
	initColTypeRowReceiverMap()
	var (
		err     error
		conCtrl ConsistencyController
	)
	tctx, conf, pool := d.tctx, d.conf, d.dbHandle
	tctx.L().Info("begin to run Dump", zap.Stringer("conf", conf))
	m := newGlobalMetadata(tctx, d.extStore, conf.Snapshot)
	repeatableRead := needRepeatableRead(conf.ServerInfo.ServerType, conf.Consistency)
	defer func() {
		if dumpErr == nil {
			_ = m.writeGlobalMetaData()
		}
	}()

	conCtrl, err = NewConsistencyController(tctx, conf, pool)
	if err != nil {
		return err
	}
	if err = conCtrl.Setup(tctx); err != nil {
		return errors.Trace(err)
	}
	// To avoid lock is not released
	defer func() {
		err = conCtrl.TearDown(tctx)
		if err != nil {
			tctx.L().Warn("fail to tear down consistency controller", zap.Error(err))
		}
	}()

	metaConn, err := createConnWithConsistency(tctx, pool, repeatableRead)
	if err != nil {
		return err
	}
	defer func() {
		_ = metaConn.Close()
	}()
	m.recordStartTime(time.Now())
	// for consistency lock, we can write snapshot info after all tables are locked.
	// the binlog pos may changed because there is still possible write between we lock tables and write master status.
	// but for the locked tables doing replication that starts from metadata is safe.
	// for consistency flush, record snapshot after whole tables are locked. The recorded meta info is exactly the locked snapshot.
	// for consistency snapshot, we should use the snapshot that we get/set at first in metadata. TiDB will assure the snapshot of TSO.
	// for consistency none, the binlog pos in metadata might be earlier than dumped data. We need to enable safe-mode to assure data safety.
	err = m.recordGlobalMetaData(metaConn, conf.ServerInfo, false)
	if err != nil {
		tctx.L().Warn("get global metadata failed", log.ShortError(err))
	}

	if err = prepareTableListToDump(tctx, conf, metaConn); err != nil {
		return err
	}

	atomic.StoreInt64(&d.totalTables, int64(calculateTableCount(conf.Tables)))

	rebuildMetaConn := func(conn *sql.Conn, updateMeta bool) (*sql.Conn, error) {
		_ = conn.Raw(func(any) error {
			// return an `ErrBadConn` to ensure close the connection, but do not put it back to the pool.
			// if we choose to use `Close`, it will always put the connection back to the pool.
			return driver.ErrBadConn
		})

		newConn, err1 := createConnWithConsistency(tctx, pool, repeatableRead)
		if err1 != nil {
			return conn, errors.Trace(err1)
		}
		conn = newConn
		// renew the master status after connection. dm can't close safe-mode until dm reaches current pos
		if updateMeta && conf.PosAfterConnect {
			err1 = m.recordGlobalMetaData(conn, conf.ServerInfo, true)
			if err1 != nil {
				return conn, errors.Trace(err1)
			}
		}
		return conn, nil
	}

	rebuildConn := func(conn *sql.Conn, updateMeta bool) (*sql.Conn, error) {
		// make sure that the lock connection is still alive
		err1 := conCtrl.PingContext(tctx)
		if err1 != nil {
			return conn, errors.Trace(err1)
		}
		return rebuildMetaConn(conn, updateMeta)
	}

	chanSize := defaultTaskChannelCapacity
	failpoint.Inject("SmallDumpChanSize", func() {
		chanSize = 1
	})
	taskIn, taskOut := infiniteChan[Task]()
	// todo: refine metrics
	AddGauge(d.metrics.taskChannelCapacity, float64(chanSize))
	wg, writingCtx := errgroup.WithContext(tctx)
	writerCtx := tctx.WithContext(writingCtx)
	writers, tearDownWriters, err := d.startWriters(writerCtx, wg, taskOut, rebuildConn)
	if err != nil {
		return err
	}
	defer tearDownWriters()

	if conf.TransactionalConsistency {
		if err = conCtrl.TearDown(tctx); err != nil {
			return errors.Trace(err)
		}
	}
	// Inject consistency failpoint test after we release the table lock
	failpoint.Inject("ConsistencyCheck", nil)

	if conf.PosAfterConnect {
		// record again, to provide a location to exit safe mode for DM
		err = m.recordGlobalMetaData(metaConn, conf.ServerInfo, true)
		if err != nil {
			tctx.L().Info("get global metadata (after connection pool established) failed", log.ShortError(err))
		}
	}

	summary.SetLogCollector(summary.NewLogCollector(tctx.L().Info))
	summary.SetUnit(summary.BackupUnit)
	defer summary.Summary(summary.BackupUnit)

	logProgressCtx, logProgressCancel := tctx.WithCancel()
	go d.runLogProgress(logProgressCtx)
	defer logProgressCancel()

	tableDataStartTime := time.Now()

	failpoint.Inject("PrintTiDBMemQuotaQuery", func(_ failpoint.Value) {
		row := d.dbHandle.QueryRowContext(tctx, "select @@tidb_mem_quota_query;")
		var s string
		err = row.Scan(&s)
		if err != nil {
			fmt.Println(errors.Trace(err))
		} else {
			fmt.Printf("tidb_mem_quota_query == %s\n", s)
		}
	})
	baseConn := newBaseConn(metaConn, true, rebuildMetaConn)

	if conf.SQL == "" {
		if err = d.dumpDatabases(writerCtx, baseConn, taskIn); err != nil && !errors.ErrorEqual(err, context.Canceled) {
			return err
		}
	} else {
		d.dumpSQL(writerCtx, baseConn, taskIn)
	}
	d.metrics.progressReady.Store(true)
	close(taskIn)
	failpoint.Inject("EnableLogProgress", func() {
		time.Sleep(1 * time.Second)
		tctx.L().Debug("progress ready, sleep 1s")
	})
	_ = baseConn.DBConn.Close()
	if err := wg.Wait(); err != nil {
		summary.CollectFailureUnit("dump table data", err)
		return errors.Trace(err)
	}
	summary.CollectSuccessUnit("dump cost", countTotalTask(writers), time.Since(tableDataStartTime))

	summary.SetSuccessStatus(true)
	m.recordFinishTime(time.Now())
	return nil
}

func (d *Dumper) startWriters(tctx *tcontext.Context, wg *errgroup.Group, taskChan <-chan Task,
	rebuildConnFn func(*sql.Conn, bool) (*sql.Conn, error)) ([]*Writer, func(), error) {
	conf, pool := d.conf, d.dbHandle
	writers := make([]*Writer, conf.Threads)
	for i := 0; i < conf.Threads; i++ {
		conn, err := createConnWithConsistency(tctx, pool, needRepeatableRead(conf.ServerInfo.ServerType, conf.Consistency))
		if err != nil {
			return nil, func() {}, err
		}
		writer := NewWriter(tctx, int64(i), conf, conn, d.extStore, d.metrics)
		writer.rebuildConnFn = rebuildConnFn
		writer.setFinishTableCallBack(func(task Task) {
			if _, ok := task.(*TaskTableData); ok {
				IncCounter(d.metrics.finishedTablesCounter)
				// FIXME: actually finishing the last chunk doesn't means this table is 'finished'.
				//  We can call this table is 'finished' if all its chunks are finished.
				//  Comment this log now to avoid ambiguity.
				// tctx.L().Debug("finished dumping table data",
				//	zap.String("database", td.Meta.DatabaseName()),
				//	zap.String("table", td.Meta.TableName()))
				failpoint.Inject("EnableLogProgress", func() {
					time.Sleep(1 * time.Second)
					tctx.L().Debug("EnableLogProgress, sleep 1s")
				})
			}
		})
		writer.setFinishTaskCallBack(func(task Task) {
			IncGauge(d.metrics.taskChannelCapacity)
			if td, ok := task.(*TaskTableData); ok {
				d.metrics.completedChunks.Add(1)
				tctx.L().Debug("finish dumping table data task",
					zap.String("database", td.Meta.DatabaseName()),
					zap.String("table", td.Meta.TableName()),
					zap.Int("chunkIdx", td.ChunkIndex))
			}
		})
		wg.Go(func() error {
			return writer.run(taskChan)
		})
		writers[i] = writer
	}
	tearDown := func() {
		for _, w := range writers {
			_ = w.conn.Close()
		}
	}
	return writers, tearDown, nil
}

func (d *Dumper) dumpDatabases(tctx *tcontext.Context, metaConn *BaseConn, taskChan chan<- Task) error {
	conf := d.conf
	allTables := conf.Tables

	for dbName, tables := range allTables {
		if !conf.NoSchemas {
			createDatabaseSQL, err := ShowCreateDatabase(tctx, metaConn, dbName)
			if err != nil {
				return errors.Trace(err)
			}

			task := NewTaskDatabaseMeta(dbName, createDatabaseSQL)
			ctxDone := d.sendTaskToChan(tctx, task, taskChan)
			if ctxDone {
				return tctx.Err()
			}
		}

		for _, table := range tables {
			tctx.L().Debug("start dumping table...", zap.String("database", dbName),
				zap.String("table", table.Name))
			meta, err := dumpTableMeta(tctx, conf, metaConn, dbName, table)
			if err != nil {
				return errors.Trace(err)
			}

			if !conf.NoSchemas {
				switch table.Type {
				case TableTypeView:
					task := NewTaskViewMeta(dbName, table.Name, meta.ShowCreateTable(), meta.ShowCreateView())
					ctxDone := d.sendTaskToChan(tctx, task, taskChan)
					if ctxDone {
						return tctx.Err()
					}
				case TableTypeSequence:
					task := NewTaskSequenceMeta(dbName, table.Name, meta.ShowCreateTable())
					ctxDone := d.sendTaskToChan(tctx, task, taskChan)
					if ctxDone {
						return tctx.Err()
					}
				default:
					task := NewTaskTableMeta(dbName, table.Name, meta.ShowCreateTable())
					ctxDone := d.sendTaskToChan(tctx, task, taskChan)
					if ctxDone {
						return tctx.Err()
					}
				}
			}
			if table.Type == TableTypeBase {
				err = d.dumpTableData(tctx, metaConn, meta, taskChan)
				if err != nil {
					return errors.Trace(err)
				}
			}
		}
	}
	return nil
}


func (d *Dumper) dumpTableData(tctx *tcontext.Context, conn *BaseConn, meta TableMeta, taskChan chan<- Task) error {
	conf := d.conf
	if conf.NoData {
		return nil
	}

	// Update total rows
	fieldName, _ := pickupPossibleField(tctx, meta, conn)
	c := estimateCount(tctx, meta.DatabaseName(), meta.TableName(), conn, fieldName, conf)
	AddCounter(d.metrics.estimateTotalRowsCounter, float64(c))

	if conf.Rows == UnspecifiedSize {
		return d.sequentialDumpTable(tctx, conn, meta, taskChan)
	}
	return d.concurrentDumpTable(tctx, conn, meta, taskChan)
}

func (d *Dumper) buildConcatTask(tctx *tcontext.Context, conn *BaseConn, meta TableMeta) (*TaskTableData, error) {
	tableChan := make(chan Task, 128)
	errCh := make(chan error, 1)
	go func() {
		// adjust rows to suitable rows for this table
		d.conf.Rows = GetSuitableRows(meta.AvgRowLength())
		err := d.concurrentDumpTable(tctx, conn, meta, tableChan)
		d.conf.Rows = UnspecifiedSize
		if err != nil {
			errCh <- err
		} else {
			close(errCh)
		}
	}()
	tableDataArr := make([]*tableData, 0)
	handleSubTask := func(task Task) {
		tableTask, ok := task.(*TaskTableData)
		if !ok {
			tctx.L().Warn("unexpected task when splitting table chunks", zap.String("task", tableTask.Brief()))
			return
		}
		tableDataInst, ok := tableTask.Data.(*tableData)
		if !ok {
			tctx.L().Warn("unexpected task.Data when splitting table chunks", zap.String("task", tableTask.Brief()))
			return
		}
		tableDataArr = append(tableDataArr, tableDataInst)
		d.metrics.totalChunks.Dec()
	}
	for {
		select {
		case err, ok := <-errCh:
			if !ok {
				// make sure all the subtasks in tableChan are handled
				for len(tableChan) > 0 {
					task := <-tableChan
					handleSubTask(task)
				}
				if len(tableDataArr) <= 1 {
					return nil, nil
				}
				queries := make([]string, 0, len(tableDataArr))
				colLen := tableDataArr[0].colLen
				for _, tableDataInst := range tableDataArr {
					queries = append(queries, tableDataInst.query)
					if colLen != tableDataInst.colLen {
						tctx.L().Warn("colLen varies for same table",
							zap.Int("oldColLen", colLen),
							zap.String("oldQuery", queries[0]),
							zap.Int("newColLen", tableDataInst.colLen),
							zap.String("newQuery", tableDataInst.query))
						return nil, nil
					}
				}
				return d.newTaskTableData(meta, newMultiQueriesChunk(queries, colLen), 0, 1), nil
			}
			return nil, err
		case task := <-tableChan:
			handleSubTask(task)
		}
	}
}

func (d *Dumper) dumpWholeTableDirectly(tctx *tcontext.Context, meta TableMeta, taskChan chan<- Task, partition, orderByClause string, currentChunk, totalChunks int) error {
	conf := d.conf
	tableIR := SelectAllFromTable(conf, meta, partition, orderByClause)
	task := d.newTaskTableData(meta, tableIR, currentChunk, totalChunks)
	ctxDone := d.sendTaskToChan(tctx, task, taskChan)
	if ctxDone {
		return tctx.Err()
	}
	return nil
}

func (d *Dumper) sequentialDumpTable(tctx *tcontext.Context, conn *BaseConn, meta TableMeta, taskChan chan<- Task) error {
	conf := d.conf
	orderByClause, err := buildOrderByClause(tctx, conf, conn, meta.DatabaseName(), meta.TableName(), meta.HasImplicitRowID())
	if err != nil {
		return err
	}
	return d.dumpWholeTableDirectly(tctx, meta, taskChan, "", orderByClause, 0, 1)
}

// concurrentDumpTable tries to split table into several chunks to dump
func (d *Dumper) concurrentDumpTable(tctx *tcontext.Context, conn *BaseConn, meta TableMeta, taskChan chan<- Task) error {
	conf := d.conf
	db, tbl := meta.DatabaseName(), meta.TableName()
	orderByClause, err := buildOrderByClause(tctx, conf, conn, db, tbl, meta.HasImplicitRowID())
	if err != nil {
		return err
	}

	field, err := pickupPossibleField(tctx, meta, conn)
	if err != nil || field == "" {
		// skip split chunk logic if not found proper field
		tctx.L().Info("fallback to sequential dump due to no proper field. This won't influence the whole dump process",
			zap.String("database", db), zap.String("table", tbl), log.ShortError(err))
		return d.dumpWholeTableDirectly(tctx, meta, taskChan, "", orderByClause, 0, 1)
	}

	count := estimateCount(d.tctx, db, tbl, conn, field, conf)
	tctx.L().Info("get estimated rows count",
		zap.String("database", db),
		zap.String("table", tbl),
		zap.Uint64("estimateCount", count))
	if count < conf.Rows {
		// skip chunk logic if estimates are low
		tctx.L().Info("fallback to sequential dump due to estimate count < rows. This won't influence the whole dump process",
			zap.Uint64("estimate count", count),
			zap.Uint64("conf.rows", conf.Rows),
			zap.String("database", db),
			zap.String("table", tbl))
		return d.dumpWholeTableDirectly(tctx, meta, taskChan, "", orderByClause, 0, 1)
	}

	minv, maxv, err := d.selectMinAndMaxIntValue(tctx, conn, db, tbl, field)
	if err != nil {
		tctx.L().Info("fallback to sequential dump due to cannot get bounding values. This won't influence the whole dump process",
			log.ShortError(err))
		return d.dumpWholeTableDirectly(tctx, meta, taskChan, "", orderByClause, 0, 1)
	}
	tctx.L().Debug("get int bounding values",
		zap.String("lower", minv.String()),
		zap.String("upper", maxv.String()))

	// every chunk would have eventual adjustments
	estimatedChunks := count / conf.Rows
	estimatedStep := new(big.Int).Sub(maxv, minv).Uint64()/estimatedChunks + 1
	bigEstimatedStep := new(big.Int).SetUint64(estimatedStep)
	cutoff := new(big.Int).Set(minv)
	totalChunks := estimatedChunks
	if estimatedStep == 1 {
		totalChunks = new(big.Int).Sub(maxv, minv).Uint64() + 1
	}

	selectField, selectLen := meta.SelectedField(), meta.SelectedLen()

	chunkIndex := 0
	nullValueCondition := ""
	if conf.Where == "" {
		nullValueCondition = fmt.Sprintf("`%s` IS NULL OR ", escapeString(field))
	}
	for maxv.Cmp(cutoff) >= 0 {
		nextCutOff := new(big.Int).Add(cutoff, bigEstimatedStep)
		where := fmt.Sprintf("%s(`%s` >= %d AND `%s` < %d)", nullValueCondition, escapeString(field), cutoff, escapeString(field), nextCutOff)
		query := buildSelectQuery(db, tbl, selectField, "", buildWhereCondition(conf, where), orderByClause)
		if len(nullValueCondition) > 0 {
			nullValueCondition = ""
		}
		task := d.newTaskTableData(meta, newTableData(query, selectLen, false), chunkIndex, int(totalChunks))
		ctxDone := d.sendTaskToChan(tctx, task, taskChan)
		if ctxDone {
			return tctx.Err()
		}
		cutoff = nextCutOff
		chunkIndex++
	}
	return nil
}

func (d *Dumper) sendTaskToChan(tctx *tcontext.Context, task Task, taskChan chan<- Task) (ctxDone bool) {
	select {
	case <-tctx.Done():
		return true
	case taskChan <- task:
		tctx.L().Debug("send task to writer",
			zap.String("task", task.Brief()))
		DecGauge(d.metrics.taskChannelCapacity)
		return false
	}
}

func (d *Dumper) selectMinAndMaxIntValue(tctx *tcontext.Context, conn *BaseConn, db, tbl, field string) (*big.Int, *big.Int, error) {
	conf, zero := d.conf, &big.Int{}
	query := fmt.Sprintf("SELECT MIN(`%s`),MAX(`%s`) FROM `%s`.`%s`",
		escapeString(field), escapeString(field), escapeString(db), escapeString(tbl))
	if conf.Where != "" {
		query = fmt.Sprintf("%s WHERE %s", query, conf.Where)
	}
	tctx.L().Debug("split chunks", zap.String("query", query))

	var smin sql.NullString
	var smax sql.NullString
	err := conn.QuerySQL(tctx, func(rows *sql.Rows) error {
		err := rows.Scan(&smin, &smax)
		rows.Close()
		return err
	}, func() {}, query)
	if err != nil {
		return zero, zero, errors.Annotatef(err, "can't get min/max values to split chunks, query: %s", query)
	}
	if !smax.Valid || !smin.Valid {
		// found no data
		return zero, zero, errors.Errorf("no invalid min/max value found in query %s", query)
	}

	maxv := new(big.Int)
	minv := new(big.Int)
	var ok bool
	if maxv, ok = maxv.SetString(smax.String, 10); !ok {
		return zero, zero, errors.Errorf("fail to convert max value %s in query %s", smax.String, query)
	}
	if minv, ok = minv.SetString(smin.String, 10); !ok {
		return zero, zero, errors.Errorf("fail to convert min value %s in query %s", smin.String, query)
	}
	return minv, maxv, nil
}

// L returns real logger
func (d *Dumper) L() log.Logger {
	return d.tctx.L()
}

func getListTableTypeByConf(conf *Config) listTableType {
	return listTableByShowTableStatus
}

func prepareTableListToDump(tctx *tcontext.Context, conf *Config, db *sql.Conn) error {
	if conf.SQL != "" {
		return nil
	}

	listType := getListTableTypeByConf(conf)

	if conf.SpecifiedTables {
		return updateSpecifiedTablesMeta(tctx, db, conf.Tables, listType)
	}
	databases, err := prepareDumpingDatabases(tctx, conf, db)
	if err != nil {
		return err
	}

	tableTypes := []TableType{TableTypeBase}
	if !conf.NoViews {
		tableTypes = append(tableTypes, TableTypeView)
	}
	if !conf.NoSequences {
		tableTypes = append(tableTypes, TableTypeSequence)
	}

	conf.Tables, err = ListAllDatabasesTables(tctx, db, databases, listType, tableTypes...)
	if err != nil {
		return err
	}

	filterTables(tctx, conf)
	return nil
}

func dumpTableMeta(tctx *tcontext.Context, conf *Config, conn *BaseConn, db string, table *TableInfo) (TableMeta, error) {
	tbl := table.Name
	selectField, selectLen, err := buildSelectField(tctx, conn, db, tbl, conf.CompleteInsert)
	if err != nil {
		return nil, err
	}
	var (
		colTypes         []*sql.ColumnType
		hasImplicitRowID bool
	)

	// If all columns are generated
	if table.Type == TableTypeBase {
		if selectField == "" {
			colTypes, err = GetColumnTypes(tctx, conn, "*", db, tbl)
		} else {
			colTypes, err = GetColumnTypes(tctx, conn, selectField, db, tbl)
		}
	}
	if err != nil {
		return nil, err
	}

	meta := &tableMeta{
		avgRowLength:     table.AvgRowLength,
		database:         db,
		table:            tbl,
		colTypes:         colTypes,
		selectedField:    selectField,
		selectedLen:      selectLen,
		hasImplicitRowID: hasImplicitRowID,
		specCmts:         getSpecialComments(conf.ServerInfo.ServerType),
	}

	if conf.NoSchemas {
		return meta, nil
	}
	switch table.Type {
	case TableTypeView:
		viewName := table.Name
		createTableSQL, createViewSQL, err1 := ShowCreateView(tctx, conn, db, viewName)
		if err1 != nil {
			return meta, err1
		}
		meta.showCreateTable = createTableSQL
		meta.showCreateView = createViewSQL
		return meta, nil
	}

	createTableSQL, err := ShowCreateTable(tctx, conn, db, tbl)
	if err != nil {
		return nil, err
	}
	meta.showCreateTable = createTableSQL
	return meta, nil
}

func (d *Dumper) dumpSQL(tctx *tcontext.Context, metaConn *BaseConn, taskChan chan<- Task) {
	conf := d.conf
	meta := &tableMeta{}
	data := newTableData(conf.SQL, 0, true)
	task := d.newTaskTableData(meta, data, 0, 1)
	c := detectEstimateRows(tctx, metaConn, fmt.Sprintf("EXPLAIN %s", conf.SQL), []string{"rows", "estRows", "count"})
	AddCounter(d.metrics.estimateTotalRowsCounter, float64(c))
	atomic.StoreInt64(&d.totalTables, int64(1))
	d.sendTaskToChan(tctx, task, taskChan)
}

func canRebuildConn(consistency string, trxConsistencyOnly bool) bool {
	switch consistency {
	case ConsistencyTypeSnapshot, ConsistencyTypeNone, ConsistencyTypeAuto:
		return true
	default:
		return false
	}
}

// Close closes a Dumper and stop dumping immediately
func (d *Dumper) Close() error {
	d.cancelCtx()
	d.metrics.unregisterFrom(d.conf.PromRegistry)
	if d.dbHandle != nil {
		return d.dbHandle.Close()
	}
	return nil
}

func runSteps(d *Dumper, steps ...func(*Dumper) error) error {
	for _, st := range steps {
		err := st(d)
		if err != nil {
			return err
		}
	}
	return nil
}

func initLogger(d *Dumper) error {
	conf := d.conf
	var (
		logger log.Logger
		err    error
		props  *pclog.ZapProperties
	)
	// conf.Logger != nil means dumpling is used as a library
	if conf.Logger != nil {
		logger = log.NewAppLogger(conf.Logger)
	} else {
		logger, props, err = log.InitAppLogger(&log.Config{
			Level:  conf.LogLevel,
			File:   conf.LogFile,
			Format: conf.LogFormat,
		})
		if err != nil {
			return errors.Trace(err)
		}
		pclog.ReplaceGlobals(logger.Logger, props)
		cli.LogLongVersion(logger)
	}
	d.tctx = d.tctx.WithLogger(logger)
	return nil
}

// createExternalStore is an initialization step of Dumper.
func createExternalStore(d *Dumper) error {
	tctx, conf := d.tctx, d.conf
	extStore, err := conf.createExternalStorage(tctx)
	if err != nil {
		return errors.Trace(err)
	}
	d.extStore = extStore
	return nil
}

// startHTTPService is an initialization step of Dumper.
func startHTTPService(d *Dumper) error {
	conf := d.conf
	if conf.StatusAddr != "" {
		go func() {
			err := startDumplingService(d.tctx, conf.StatusAddr)
			if err != nil {
				d.L().Info("meet error when stopping dumpling http service", log.ShortError(err))
			}
		}()
	}
	return nil
}

// openSQLDB is an initialization step of Dumper.
func openSQLDB(d *Dumper) error {
	conf := d.conf
	db, err := sql.Open("pgx", conf.GetDSN(""))
	if err != nil {
		return errors.Trace(err)
	}
	d.dbHandle = db
	return nil
}

func resolveAutoConsistency(d *Dumper) error {
	conf := d.conf
	if conf.Consistency == ConsistencyTypeAuto {
		conf.Consistency = ConsistencyTypeSnapshot
	}
	return nil
}

func validateResolveAutoConsistency(d *Dumper) error {
	conf := d.conf
	if conf.Consistency != ConsistencyTypeSnapshot && conf.Snapshot != "" {
		return errors.Errorf("can't specify --snapshot when --consistency isn't snapshot, resolved consistency: %s", conf.Consistency)
	}
	return nil
}

// setSessionParam is an initialization step of Dumper.
func setSessionParam(d *Dumper) error {
	conf, pool := d.conf, d.dbHandle
	var err error
	if d.dbHandle, err = resetDBWithSessionParams(d.tctx, pool, conf.GetDSN(""), conf.SessionParams); err != nil {
		return errors.Trace(err)
	}
	return nil
}



func (d *Dumper) newTaskTableData(meta TableMeta, data TableDataIR, currentChunk, totalChunks int) *TaskTableData {
	d.metrics.totalChunks.Add(1)
	return NewTaskTableData(meta, data, currentChunk, totalChunks)
}
