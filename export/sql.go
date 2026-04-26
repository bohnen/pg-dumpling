// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	tcontext "github.com/tadapin/pg-dumpling/context"
	"github.com/tadapin/pg-dumpling/log"
	"go.uber.org/zap"
)

// pgDumpEnv carries the connection parameters that runPgDumpSchema passes
// to the spawned pg_dump process. Set once at startup from the dumper's
// configuration so we don't have to thread *Config into every call site.
var pgDumpEnv []string

// SetPgDumpEnv is called once during dumper init to record the host/port/
// user/password/sslmode that the running pg_dump child should use. It is
// expressed as os.Environ()-style "KEY=VALUE" entries (typically PGHOST,
// PGPORT, PGUSER, PGPASSWORD, PGSSLMODE).
func SetPgDumpEnv(env []string) { pgDumpEnv = env }

// runPgDumpSchema invokes `pg_dump --schema-only --no-owner --no-privileges
// -t schema.table -d database` and returns the captured stdout. If
// includeView is true the same call returns the DDL for a view (pg_dump
// happily dumps view definitions when they match -t).
func runPgDumpSchema(tctx *tcontext.Context, schema, name string, includeView bool) (string, error) {
	target := fmt.Sprintf("%s.%s", schema, name)
	args := []string{"--schema-only", "--no-owner", "--no-privileges", "-t", target}
	cmd := exec.CommandContext(tctx, "pg_dump", args...)
	cmd.Env = append(append([]string{}, osEnviron()...), pgDumpEnv...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", errors.Annotatef(err, "pg_dump %s failed: %s", target, string(ee.Stderr))
		}
		return "", errors.Annotatef(err, "pg_dump %s", target)
	}
	_ = includeView
	return string(out), nil
}

// osEnviron is split out so tests can override it.
var osEnviron = func() []string { return os.Environ() }

// pgQuoteIdent quotes a PostgreSQL identifier following the standard rules:
// wrap in double quotes and double any embedded double quote.
func pgQuoteIdent(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 2)
	b.WriteByte('"')
	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			b.WriteByte('"')
		}
		b.WriteByte(name[i])
	}
	b.WriteByte('"')
	return b.String()
}

// pgQuoteQName quotes "schema"."name".
func pgQuoteQName(schema, name string) string {
	if schema == "" {
		return pgQuoteIdent(name)
	}
	return pgQuoteIdent(schema) + "." + pgQuoteIdent(name)
}

const (
	orderByTiDBRowID = "ORDER BY `_tidb_rowid`"
	snapshotVar      = "tidb_snapshot"
)

type listTableType int

const (
	listTableByInfoSchema listTableType = iota
	listTableByShowFullTables
	listTableByShowTableStatus
)

// ShowDatabases lists the user-visible PostgreSQL schemas. System schemas
// (pg_catalog, information_schema, pg_toast, pg_temp_*, pg_toast_temp_*) are
// excluded.
func ShowDatabases(db *sql.Conn) ([]string, error) {
	const q = `SELECT nspname FROM pg_catalog.pg_namespace
		WHERE nspname NOT IN ('pg_catalog','information_schema','pg_toast')
		  AND nspname NOT LIKE 'pg_temp_%'
		  AND nspname NOT LIKE 'pg_toast_temp_%'
		ORDER BY nspname`
	var res oneStrColumnTable
	if err := simpleQuery(db, q, res.handleOneRow); err != nil {
		return nil, err
	}
	return res.data, nil
}

// ShowTables lists the tables in the user's search_path schemas. Callers
// usually want ListAllDatabasesTables instead, which scopes by schema.
func ShowTables(db *sql.Conn) ([]string, error) {
	const q = `SELECT format('%I.%I', schemaname, tablename) FROM pg_catalog.pg_tables
		WHERE schemaname NOT IN ('pg_catalog','information_schema','pg_toast')
		  AND schemaname NOT LIKE 'pg_temp_%'
		ORDER BY schemaname, tablename`
	var res oneStrColumnTable
	if err := simpleQuery(db, q, res.handleOneRow); err != nil {
		return nil, err
	}
	return res.data, nil
}

// ShowCreateDatabase emits a CREATE SCHEMA statement for the given schema.
// In Postgres there's no "SHOW CREATE DATABASE" equivalent that produces the
// owner/encoding hints; we just emit a permissive CREATE SCHEMA so the dump
// can be loaded into a fresh database.
func ShowCreateDatabase(_ *tcontext.Context, _ *BaseConn, database string) (string, error) {
	return fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, pgQuoteIdent(database)), nil
}

// ShowCreateTable shells out to pg_dump --schema-only to get the DDL for a
// specific table. Connection parameters are taken from PG-standard env vars
// (PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE) or from the conn's DSN
// when set via SetPgDumpEnv. The tctx is used for cancellation and logging
// only; the underlying *BaseConn isn't actually queried.
func ShowCreateTable(tctx *tcontext.Context, _ *BaseConn, database, table string) (string, error) {
	return runPgDumpSchema(tctx, database, table, false)
}

// ShowCreateView shells out to pg_dump for a view's DDL. The first return
// (createFakeTableSQL) is empty for Postgres — PG views don't need the MySQL
// "fake table" preamble that lets a downstream loader resolve circular
// dependencies; pg_dump emits the view in dependency order itself.
func ShowCreateView(tctx *tcontext.Context, _ *BaseConn, database, view string) (createFakeTableSQL string, createRealViewSQL string, err error) {
	ddl, err := runPgDumpSchema(tctx, database, view, true)
	if err != nil {
		return "", "", err
	}
	return "", ddl, nil
}

// updateSpecifiedTablesMeta fills in Type and AvgRowLength for the
// already-collected (schema, table) entries in dbTables. listType is ignored
// in the PG implementation; we always use the same pg_class / pg_namespace
// query.
func updateSpecifiedTablesMeta(tctx *tcontext.Context, db *sql.Conn, dbTables DatabaseTables, _ listTableType) error {
	const q = `
SELECT n.nspname,
       c.relname,
       CASE c.relkind
         WHEN 'r' THEN 'BASE TABLE'
         WHEN 'p' THEN 'BASE TABLE'
         WHEN 'v' THEN 'VIEW'
         WHEN 'm' THEN 'VIEW'
         WHEN 'S' THEN 'SEQUENCE'
         ELSE 'BASE TABLE'
       END AS table_type,
       COALESCE(
         CASE WHEN c.reltuples > 0
              THEN (pg_relation_size(c.oid) / c.reltuples)::bigint
              ELSE 0
         END, 0) AS avg_row_length
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1) AND c.relname = ANY($2)`
	schemas := make([]string, 0, len(dbTables))
	tables := make([]string, 0)
	for schema, tbls := range dbTables {
		schemas = append(schemas, schema)
		for _, tbl := range tbls {
			tables = append(tables, tbl.Name)
		}
	}
	rows, err := db.QueryContext(tctx, q, anyStringArray(schemas), anyStringArray(tables))
	if err != nil {
		return errors.Annotatef(err, "sql: %s", q)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			schema, name, ttype string
			avgRowLength        sql.NullInt64
		)
		if err := rows.Scan(&schema, &name, &ttype, &avgRowLength); err != nil {
			return errors.Trace(err)
		}
		tableType, err := ParseTableType(ttype)
		if err != nil {
			return errors.Trace(err)
		}
		for _, tbl := range dbTables[schema] {
			if tbl.Name == name {
				tbl.Type = tableType
				if avgRowLength.Valid {
					tbl.AvgRowLength = uint64(avgRowLength.Int64)
				}
			}
		}
	}
	return rows.Err()
}

// anyStringArray converts a Go []string into the pgx-friendly form for
// `WHERE col = ANY($1)` predicates. pgx accepts []string directly so we can
// just return it; the helper exists to make call sites read clearly.
func anyStringArray(s []string) []string { return s }

// ListAllDatabasesTables enumerates the tables/views inside the given
// schemas, filtered to the requested TableType set. Always uses the same
// pg_class JOIN; listType is ignored.
func ListAllDatabasesTables(tctx *tcontext.Context, db *sql.Conn, databaseNames []string,
	_ listTableType, tableTypes ...TableType) (DatabaseTables, error) {
	dbTables := DatabaseTables{}
	for _, schema := range databaseNames {
		dbTables[schema] = make([]*TableInfo, 0)
	}
	wantType := make(map[TableType]struct{}, len(tableTypes))
	for _, t := range tableTypes {
		wantType[t] = struct{}{}
	}
	const q = `
SELECT n.nspname,
       c.relname,
       CASE c.relkind
         WHEN 'r' THEN 'BASE TABLE'
         WHEN 'p' THEN 'BASE TABLE'
         WHEN 'v' THEN 'VIEW'
         WHEN 'm' THEN 'VIEW'
         WHEN 'S' THEN 'SEQUENCE'
         ELSE 'BASE TABLE'
       END AS table_type,
       COALESCE(
         CASE WHEN c.reltuples > 0
              THEN (pg_relation_size(c.oid) / c.reltuples)::bigint
              ELSE 0
         END, 0) AS avg_row_length
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1)
  AND c.relkind = ANY(ARRAY['r','p','v','m'])
ORDER BY n.nspname, c.relname`
	rows, err := db.QueryContext(tctx, q, anyStringArray(databaseNames))
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", q)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			schema, name, ttype string
			avgRowLength        sql.NullInt64
		)
		if err := rows.Scan(&schema, &name, &ttype, &avgRowLength); err != nil {
			return nil, errors.Trace(err)
		}
		tableType, err := ParseTableType(ttype)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if _, ok := wantType[tableType]; !ok && len(wantType) > 0 {
			continue
		}
		var avg uint64
		if avgRowLength.Valid {
			avg = uint64(avgRowLength.Int64)
		}
		dbTables[schema] = append(dbTables[schema], &TableInfo{name, avg, tableType})
	}
	return dbTables, rows.Err()
}

// SelectVersion gets the version information from the database server
func SelectVersion(db *sql.DB) (string, error) {
	var versionInfo string
	const query = "SELECT version()"
	row := db.QueryRow(query)
	err := row.Scan(&versionInfo)
	if err != nil {
		return "", errors.Annotatef(err, "sql: %s", query)
	}
	return versionInfo, nil
}

// SelectAllFromTable dumps data serialized from a specified table
func SelectAllFromTable(conf *Config, meta TableMeta, partition, orderByClause string) TableDataIR {
	database, table := meta.DatabaseName(), meta.TableName()
	selectedField, selectLen := meta.SelectedField(), meta.SelectedLen()
	query := buildSelectQuery(database, table, selectedField, partition, buildWhereCondition(conf, ""), orderByClause)

	return &tableData{
		query:  query,
		colLen: selectLen,
	}
}

func buildSelectQuery(database, table, fields, partition, where, orderByClause string) string {
	var query strings.Builder
	query.WriteString("SELECT ")
	if fields == "" {
		fields = "''"
	}
	query.WriteString(fields)
	query.WriteString(" FROM ")
	if partition != "" {
		// Postgres declarative partitioning: each partition is a regular table
		// in the same schema. Reach into it directly.
		query.WriteString(pgQuoteQName(database, partition))
	} else {
		query.WriteString(pgQuoteQName(database, table))
	}
	if where != "" {
		query.WriteString(" ")
		query.WriteString(where)
	}
	if orderByClause != "" {
		query.WriteString(" ")
		query.WriteString(orderByClause)
	}
	return query.String()
}

func buildOrderByClause(tctx *tcontext.Context, conf *Config, db *BaseConn, database, table string, hasImplicitRowID bool) (string, error) { // revive:disable-line:flag-parameter
	if !conf.SortByPk {
		return "", nil
	}
	if hasImplicitRowID {
		return orderByTiDBRowID, nil
	}
	cols, err := GetPrimaryKeyColumns(tctx, db, database, table)
	if err != nil {
		return "", errors.Trace(err)
	}
	return buildOrderByClauseString(cols), nil
}

// GetSuitableRows gets suitable rows for each table
func GetSuitableRows(avgRowLength uint64) uint64 {
	const (
		defaultRows  = 200000
		maxRows      = 1000000
		bytesPerFile = 128 * 1024 * 1024 // 128MB per file by default
	)
	if avgRowLength == 0 {
		return defaultRows
	}
	estimateRows := bytesPerFile / avgRowLength
	if estimateRows > maxRows {
		return maxRows
	}
	return estimateRows
}

// GetColumnTypes gets *sql.ColumnTypes from a specified table
func GetColumnTypes(tctx *tcontext.Context, db *BaseConn, fields, database, table string) ([]*sql.ColumnType, error) {
	query := fmt.Sprintf("SELECT %s FROM %s LIMIT 1", fields, pgQuoteQName(database, table))
	var colTypes []*sql.ColumnType
	err := db.QuerySQL(tctx, func(rows *sql.Rows) error {
		var err error
		colTypes, err = rows.ColumnTypes()
		if err == nil {
			err = rows.Close()
		}
		failpoint.Inject("ChaosBrokenMetaConn", func(_ failpoint.Value) {
			failpoint.Return(errors.New("connection is closed"))
		})
		return errors.Annotatef(err, "sql: %s", query)
	}, func() {
		colTypes = nil
	}, query)
	if err != nil {
		return nil, err
	}
	return colTypes, nil
}

// GetPrimaryKeyAndColumnTypes gets all primary columns and their types in ordinal order
func GetPrimaryKeyAndColumnTypes(tctx *tcontext.Context, conn *BaseConn, meta TableMeta) ([]string, []string, error) {
	var (
		colNames, colTypes []string
		err                error
	)
	colNames, err = GetPrimaryKeyColumns(tctx, conn, meta.DatabaseName(), meta.TableName())
	if err != nil {
		return nil, nil, err
	}
	colName2Type := string2Map(meta.ColumnNames(), meta.ColumnTypes())
	colTypes = make([]string, len(colNames))
	for i, colName := range colNames {
		colTypes[i] = colName2Type[colName]
	}
	return colNames, colTypes, nil
}

// GetPrimaryKeyColumns gets all primary key columns in ordinal order.
func GetPrimaryKeyColumns(tctx *tcontext.Context, db *BaseConn, database, table string) ([]string, error) {
	const q = `
SELECT a.attname
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
                              AND a.attnum = ANY(i.indkey)
WHERE i.indisprimary
  AND n.nspname = $1
  AND c.relname = $2
ORDER BY array_position(i.indkey, a.attnum)`
	results, err := db.QuerySQLWithColumns(tctx, []string{"attname"}, q, database, table)
	if err != nil {
		return nil, err
	}
	cols := make([]string, 0, len(results))
	for _, oneRow := range results {
		cols = append(cols, oneRow[0])
	}
	return cols, nil
}

// getNumericIndex picks an integer-typed column from PRIMARY/unique indexes
// for use as a chunking key. Phase 1 returns "" so callers fall back to
// sequential dumping. Phase 2 will revive ctid- or PK-range chunking.
func getNumericIndex(_ *tcontext.Context, _ *BaseConn, _ TableMeta) (string, error) {
	return "", nil
}

// GetSpecifiedColumnValueAndClose get columns' values whose name is equal to columnName and close the given rows
func GetSpecifiedColumnValueAndClose(rows *sql.Rows, columnName string) ([]string, error) {
	if rows == nil {
		return []string{}, nil
	}
	defer rows.Close()
	var strs []string
	columns, _ := rows.Columns()
	addr := make([]any, len(columns))
	oneRow := make([]sql.NullString, len(columns))
	fieldIndex := -1
	for i, col := range columns {
		if strings.EqualFold(col, columnName) {
			fieldIndex = i
		}
		addr[i] = &oneRow[i]
	}
	if fieldIndex == -1 {
		return strs, nil
	}
	for rows.Next() {
		err := rows.Scan(addr...)
		if err != nil {
			return strs, errors.Trace(err)
		}
		if oneRow[fieldIndex].Valid {
			strs = append(strs, oneRow[fieldIndex].String)
		}
	}
	return strs, errors.Trace(rows.Err())
}

// GetSpecifiedColumnValuesAndClose get columns' values whose name is equal to columnName
func GetSpecifiedColumnValuesAndClose(rows *sql.Rows, columnName ...string) ([][]string, error) {
	if rows == nil {
		return [][]string{}, nil
	}
	defer rows.Close()
	var strs [][]string
	columns, err := rows.Columns()
	if err != nil {
		return strs, errors.Trace(err)
	}
	addr := make([]any, len(columns))
	oneRow := make([]sql.NullString, len(columns))
	fieldIndexMp := make(map[int]int)
	for i, col := range columns {
		addr[i] = &oneRow[i]
		for j, name := range columnName {
			if strings.EqualFold(col, name) {
				fieldIndexMp[i] = j
			}
		}
	}
	if len(fieldIndexMp) == 0 {
		return strs, nil
	}
	for rows.Next() {
		err := rows.Scan(addr...)
		if err != nil {
			return strs, errors.Trace(err)
		}
		written := false
		tmpStr := make([]string, len(columnName))
		for colPos, namePos := range fieldIndexMp {
			if oneRow[colPos].Valid {
				written = true
				tmpStr[namePos] = oneRow[colPos].String
			}
		}
		if written {
			strs = append(strs, tmpStr)
		}
	}
	return strs, errors.Trace(rows.Err())
}

// snapshotFieldIndex is the column index of the snapshot field in
// "SHOW MASTER STATUS" / "SHOW BINARY LOG STATUS" output. Retained as a local
// constant after the consistency rewrite removed it from consistency.go.
const snapshotFieldIndex = 1

// resetDBWithSessionParams returns a new *sql.DB whose default session has
// all of params applied via SET commands. The original db is closed on success.
//
// Each connection picked up from the new pool will inherit the SETs because
// pgx exposes the session-state SETs through its prepare/execution model when
// the same connection is reused; for full guarantee callers issue the SETs
// per-connection in the dump loop.
func resetDBWithSessionParams(tctx *tcontext.Context, db *sql.DB, dsn string, params map[string]any) (*sql.DB, error) {
	for k, v := range params {
		s := fmt.Sprintf("SET SESSION %s = $1", k)
		_, err := db.ExecContext(tctx, s, fmt.Sprintf("%v", v))
		if err != nil {
			if strings.Contains(err.Error(), "unrecognized configuration parameter") ||
				strings.Contains(err.Error(), "Unknown system variable") {
				tctx.L().Info("session variable is not supported by db",
					zap.String("variable", k), zap.Reflect("value", v))
				continue
			}
			return nil, errors.Trace(err)
		}
	}

	failpoint.Inject("SkipResetDB", func(_ failpoint.Value) {
		failpoint.Return(db, nil)
	})

	db.Close()
	newDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.Trace(err)
	}
	// ping to make sure the new pool is alive
	err = newDB.PingContext(tctx)
	if err != nil {
		newDB.Close()
	}
	return newDB, nil
}

// createConnWithConsistency opens a worker connection and starts it inside
// a REPEATABLE READ READ ONLY transaction. If pgSnapshotToken is non-empty
// the connection is bound to that exported snapshot so all worker
// connections observe the same MVCC view.
//
// The repeatableRead flag is kept for API compatibility; in PG mode we
// always begin a REPEATABLE READ transaction when consistency != none.
var pgSnapshotToken string

func createConnWithConsistency(ctx context.Context, db *sql.DB, repeatableRead bool) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !repeatableRead {
		return conn, nil
	}
	begin := "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"
	if _, err = conn.ExecContext(ctx, begin); err != nil {
		return nil, errors.Annotatef(err, "sql: %s", begin)
	}
	if pgSnapshotToken != "" {
		set := "SET TRANSACTION SNAPSHOT '" + escapeStringLiteral(pgSnapshotToken) + "'"
		if _, err = conn.ExecContext(ctx, set); err != nil {
			return nil, errors.Annotatef(err, "sql: %s", set)
		}
	}
	return conn, nil
}

// SetPgSnapshotToken records the token (from pg_export_snapshot()) that
// future worker connections opened via createConnWithConsistency should
// adopt with SET TRANSACTION SNAPSHOT.
func SetPgSnapshotToken(token string) { pgSnapshotToken = token }

// pgMigrationCast wraps a column reference in a server-side cast that produces
// a MySQL/TiDB-friendly text representation, based on the column's PostgreSQL
// type. The wrapped expression is aliased back to the original column name so
// downstream rows.Columns() / writer code keeps working.
//
// dataType is the value of information_schema.columns.data_type; udtName is
// the udt_name and disambiguates data_type='USER-DEFINED' (enum, pgvector,
// hstore, ranges).
//
// For types that don't need rewriting (integer, numeric, text, varchar, ...)
// the bare quoted reference is returned unchanged.
func pgMigrationCast(colName, dataType, udtName string) string {
	q := pgQuoteIdent(colName)
	alias := " AS " + q
	switch dataType {
	case "timestamp with time zone":
		return "to_char(" + q + " AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US')" + alias
	case "timestamp without time zone":
		return "to_char(" + q + ", 'YYYY-MM-DD HH24:MI:SS.US')" + alias
	case "date":
		return "to_char(" + q + ", 'YYYY-MM-DD')" + alias
	case "time without time zone":
		// PG has no to_char(time, text); cast to text. The textual form
		// is HH24:MI:SS[.fractional] with 0-6 fractional digits — MySQL
		// TIME(6) accepts that range.
		return "(" + q + ")::text" + alias
	case "time with time zone":
		// "timetz AT TIME ZONE 'UTC'" returns timetz (per PG docs), so we
		// have to explicitly cast away the TZ with ::time before stringifying.
		return "((" + q + " AT TIME ZONE 'UTC')::time)::text" + alias
	case "interval":
		return "EXTRACT(EPOCH FROM " + q + ")::numeric" + alias
	case "boolean":
		return "(" + q + ")::int" + alias
	case "bytea":
		return "encode(" + q + ", 'hex')" + alias
	case "uuid", "json", "jsonb", "inet", "cidr", "macaddr", "macaddr8",
		"xml", "money":
		return "(" + q + ")::text" + alias
	case "ARRAY":
		return "to_jsonb(" + q + ")::text" + alias
	case "tsvector", "tsquery":
		return "(" + q + ")::text" + alias
	case "USER-DEFINED":
		switch udtName {
		case "int4range", "int8range", "numrange", "tsrange", "tstzrange",
			"daterange", "int4multirange", "int8multirange", "nummultirange",
			"tsmultirange", "tstzmultirange", "datemultirange":
			return "to_jsonb(" + q + ")::text" + alias
		}
		// Enums, pgvector (`vector`), hstore, custom domains: stringify.
		// pgvector's text form '[1,2,3]' is directly compatible with TiDB
		// VECTOR.
		return "(" + q + ")::text" + alias
	}
	return q
}

// buildSelectField returns the selecting fields' string (comma-joined) and
// the number of writable columns. Reads from information_schema.columns and
// drops STORED/ALWAYS GENERATED columns from the projection so the dumped
// INSERT statements remain valid on reload.
//
// When migrationCasts is true (CSV mode for PG → MySQL/TiDB migration) each
// non-trivial column is wrapped via pgMigrationCast so the values arrive at
// the writer in a form MySQL/TiDB can ingest. In SQL mode the cast is skipped
// to keep PG → PG round-trip lossless.
func buildSelectField(tctx *tcontext.Context, db *BaseConn, dbName, tableName string, completeInsert, migrationCasts bool) (string, int, error) {
	const q = `
SELECT column_name, is_generated, data_type, udt_name
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2
ORDER BY ordinal_position`
	results, err := db.QuerySQLWithColumns(tctx,
		[]string{"column_name", "is_generated", "data_type", "udt_name"},
		q, dbName, tableName)
	if err != nil {
		return "", 0, err
	}
	availableFields := make([]string, 0, len(results))
	hasGenerateColumn := false
	for _, oneRow := range results {
		fieldName, isGen, dataType, udtName := oneRow[0], oneRow[1], oneRow[2], oneRow[3]
		if isGen == "ALWAYS" {
			hasGenerateColumn = true
			continue
		}
		var expr string
		if migrationCasts {
			expr = pgMigrationCast(fieldName, dataType, udtName)
		} else {
			expr = pgQuoteIdent(fieldName)
		}
		availableFields = append(availableFields, expr)
	}
	// Migration casts rename column expressions, so we have to spell them
	// out in the SELECT (no '*' shortcut).
	if completeInsert || hasGenerateColumn || migrationCasts {
		return strings.Join(availableFields, ","), len(availableFields), nil
	}
	return "*", len(availableFields), nil
}

func buildWhereClauses(handleColNames []string, handleVals [][]string) []string {
	if len(handleColNames) == 0 || len(handleVals) == 0 {
		return nil
	}
	quotaCols := make([]string, len(handleColNames))
	for i, s := range handleColNames {
		quotaCols[i] = pgQuoteIdent(s)
	}
	where := make([]string, 0, len(handleVals)+1)
	buf := &bytes.Buffer{}
	buildCompareClause(buf, quotaCols, handleVals[0], less, false)
	where = append(where, buf.String())
	buf.Reset()
	for i := 1; i < len(handleVals); i++ {
		low, up := handleVals[i-1], handleVals[i]
		buildBetweenClause(buf, quotaCols, low, up)
		where = append(where, buf.String())
		buf.Reset()
	}
	buildCompareClause(buf, quotaCols, handleVals[len(handleVals)-1], greater, true)
	where = append(where, buf.String())
	buf.Reset()
	return where
}

// return greater than TableRangeScan where clause
// the result doesn't contain brackets
const (
	greater = '>'
	less    = '<'
	equal   = '='
)

// buildCompareClause build clause with specified bounds. Usually we will use the following two conditions:
// (compare, writeEqual) == (less, false), return quotaCols < bound clause. In other words, (-inf, bound)
// (compare, writeEqual) == (greater, true), return quotaCols >= bound clause. In other words, [bound, +inf)
func buildCompareClause(buf *bytes.Buffer, quotaCols []string, bound []string, compare byte, writeEqual bool) { // revive:disable-line:flag-parameter
	for i, col := range quotaCols {
		if i > 0 {
			buf.WriteString("or(")
		}
		for j := 0; j < i; j++ {
			buf.WriteString(quotaCols[j])
			buf.WriteByte(equal)
			buf.WriteString(bound[j])
			buf.WriteString(" and ")
		}
		buf.WriteString(col)
		buf.WriteByte(compare)
		if writeEqual && i == len(quotaCols)-1 {
			buf.WriteByte(equal)
		}
		buf.WriteString(bound[i])
		if i > 0 {
			buf.WriteByte(')')
		} else if i != len(quotaCols)-1 {
			buf.WriteByte(' ')
		}
	}
}

// getCommonLength returns the common length of low and up
func getCommonLength(low []string, up []string) int {
	for i := range low {
		if low[i] != up[i] {
			return i
		}
	}
	return len(low)
}

// buildBetweenClause build clause in a specified table range.
// the result where clause will be low <= quotaCols < up. In other words, [low, up)
func buildBetweenClause(buf *bytes.Buffer, quotaCols []string, low []string, up []string) {
	singleBetween := func(writeEqual bool) {
		buf.WriteString(quotaCols[0])
		buf.WriteByte(greater)
		if writeEqual {
			buf.WriteByte(equal)
		}
		buf.WriteString(low[0])
		buf.WriteString(" and ")
		buf.WriteString(quotaCols[0])
		buf.WriteByte(less)
		buf.WriteString(up[0])
	}
	// handle special cases with common prefix
	commonLen := getCommonLength(low, up)
	if commonLen > 0 {
		// unexpected case for low == up, return empty result
		if commonLen == len(low) {
			buf.WriteString("false")
			return
		}
		for i := 0; i < commonLen; i++ {
			if i > 0 {
				buf.WriteString(" and ")
			}
			buf.WriteString(quotaCols[i])
			buf.WriteByte(equal)
			buf.WriteString(low[i])
		}
		buf.WriteString(" and(")
		defer buf.WriteByte(')')
		quotaCols = quotaCols[commonLen:]
		low = low[commonLen:]
		up = up[commonLen:]
	}

	// handle special cases with only one column
	if len(quotaCols) == 1 {
		singleBetween(true)
		return
	}
	buf.WriteByte('(')
	singleBetween(false)
	buf.WriteString(")or(")
	buf.WriteString(quotaCols[0])
	buf.WriteByte(equal)
	buf.WriteString(low[0])
	buf.WriteString(" and(")
	buildCompareClause(buf, quotaCols[1:], low[1:], greater, true)
	buf.WriteString("))or(")
	buf.WriteString(quotaCols[0])
	buf.WriteByte(equal)
	buf.WriteString(up[0])
	buf.WriteString(" and(")
	buildCompareClause(buf, quotaCols[1:], up[1:], less, false)
	buf.WriteString("))")
}

func buildOrderByClauseString(handleColNames []string) string {
	if len(handleColNames) == 0 {
		return ""
	}
	quotaCols := make([]string, len(handleColNames))
	for i, col := range handleColNames {
		quotaCols[i] = pgQuoteIdent(col)
	}
	return fmt.Sprintf("ORDER BY %s", strings.Join(quotaCols, ","))
}

type oneStrColumnTable struct {
	data []string
}

func (o *oneStrColumnTable) handleOneRow(rows *sql.Rows) error {
	var str string
	if err := rows.Scan(&str); err != nil {
		return errors.Trace(err)
	}
	o.data = append(o.data, str)
	return nil
}

func simpleQuery(conn *sql.Conn, query string, handleOneRow func(*sql.Rows) error) error {
	return simpleQueryWithArgs(context.Background(), conn, handleOneRow, query)
}

func simpleQueryWithArgs(ctx context.Context, conn *sql.Conn, handleOneRow func(*sql.Rows) error, query string, args ...any) error {
	var (
		rows *sql.Rows
		err  error
	)
	if len(args) > 0 {
		rows, err = conn.QueryContext(ctx, query, args...)
	} else {
		rows, err = conn.QueryContext(ctx, query)
	}
	if err != nil {
		return errors.Annotatef(err, "sql: %s, args: %s", query, args)
	}
	defer rows.Close()

	for rows.Next() {
		if err := handleOneRow(rows); err != nil {
			rows.Close()
			return errors.Annotatef(err, "sql: %s, args: %s", query, args)
		}
	}
	return errors.Annotatef(rows.Err(), "sql: %s, args: %s", query, args)
}

func pickupPossibleField(tctx *tcontext.Context, meta TableMeta, db *BaseConn) (string, error) {
	// try using _tidb_rowid first
	if meta.HasImplicitRowID() {
		return "_tidb_rowid", nil
	}
	// try to use pk or uk
	fieldName, err := getNumericIndex(tctx, db, meta)
	if err != nil {
		return "", err
	}

	// if fieldName == "", there is no proper index
	return fieldName, nil
}

func estimateCount(tctx *tcontext.Context, dbName, tableName string, db *BaseConn, field string, conf *Config) uint64 {
	var query string
	qname := pgQuoteQName(dbName, tableName)
	if strings.TrimSpace(field) == "*" || strings.TrimSpace(field) == "" {
		query = fmt.Sprintf("EXPLAIN SELECT * FROM %s", qname)
	} else {
		query = fmt.Sprintf("EXPLAIN SELECT %s FROM %s", pgQuoteIdent(field), qname)
	}

	if conf.Where != "" {
		query += " WHERE "
		query += conf.Where
	}

	estRows := detectEstimateRows(tctx, db, query, []string{"rows", "estRows", "count"})
	/* tidb results field name is estRows (before 4.0.0-beta.2: count)
		+-----------------------+----------+-----------+---------------------------------------------------------+
		| id                    | estRows  | task      | access object | operator info                           |
		+-----------------------+----------+-----------+---------------------------------------------------------+
		| tablereader_5         | 10000.00 | root      |               | data:tablefullscan_4                    |
		| └─tablefullscan_4     | 10000.00 | cop[tikv] | table:a       | table:a, keep order:false, stats:pseudo |
		+-----------------------+----------+-----------+----------------------------------------------------------

	mariadb result field name is rows
		+------+-------------+---------+-------+---------------+------+---------+------+----------+-------------+
		| id   | select_type | table   | type  | possible_keys | key  | key_len | ref  | rows     | Extra       |
		+------+-------------+---------+-------+---------------+------+---------+------+----------+-------------+
		|    1 | SIMPLE      | sbtest1 | index | NULL          | k_1  | 4       | NULL | 15000049 | Using index |
		+------+-------------+---------+-------+---------------+------+---------+------+----------+-------------+

	mysql result field name is rows
		+----+-------------+-------+------------+-------+---------------+-----------+---------+------+------+----------+-------------+
		| id | select_type | table | partitions | type  | possible_keys | key       | key_len | ref  | rows | filtered | Extra       |
		+----+-------------+-------+------------+-------+---------------+-----------+---------+------+------+----------+-------------+
		|  1 | SIMPLE      | t1    | NULL       | index | NULL          | multi_col | 10      | NULL |    5 |   100.00 | Using index |
		+----+-------------+-------+------------+-------+---------------+-----------+---------+------+------+----------+-------------+
	*/
	if estRows > 0 {
		return estRows
	}
	return 0
}

func detectEstimateRows(tctx *tcontext.Context, db *BaseConn, query string, fieldNames []string) uint64 {
	var (
		fieldIndex int
		oneRow     []sql.NullString
	)
	err := db.QuerySQL(tctx, func(rows *sql.Rows) error {
		columns, err := rows.Columns()
		if err != nil {
			return errors.Trace(err)
		}
		addr := make([]any, len(columns))
		oneRow = make([]sql.NullString, len(columns))
		fieldIndex = -1
	found:
		for i := range oneRow {
			for _, fieldName := range fieldNames {
				if strings.EqualFold(columns[i], fieldName) {
					fieldIndex = i
					break found
				}
			}
		}
		if fieldIndex == -1 {
			rows.Close()
			return nil
		}

		for i := range oneRow {
			addr[i] = &oneRow[i]
		}
		return rows.Scan(addr...)
	}, func() {}, query)
	if err != nil || fieldIndex == -1 {
		tctx.L().Info("can't estimate rows from db",
			zap.String("query", query), zap.Int("fieldIndex", fieldIndex), log.ShortError(err))
		return 0
	}

	estRows, err := strconv.ParseFloat(oneRow[fieldIndex].String, 64)
	if err != nil {
		tctx.L().Info("can't get parse estimate rows from db",
			zap.String("query", query), zap.String("estRows", oneRow[fieldIndex].String), log.ShortError(err))
		return 0
	}
	return uint64(estRows)
}

func buildWhereCondition(conf *Config, where string) string {
	var query strings.Builder
	separator := "WHERE"
	leftBracket := " "
	rightBracket := " "
	if conf.Where != "" && where != "" {
		leftBracket = " ("
		rightBracket = ") "
	}
	if conf.Where != "" {
		query.WriteString(separator)
		query.WriteString(leftBracket)
		query.WriteString(conf.Where)
		query.WriteString(rightBracket)
		separator = "AND"
	}
	if where != "" {
		query.WriteString(separator)
		query.WriteString(leftBracket)
		query.WriteString(where)
		query.WriteString(rightBracket)
	}
	return query.String()
}

func escapeString(s string) string {
	return strings.ReplaceAll(s, "`", "``")
}

// GetPartitionNames returns the child partition tables of a PostgreSQL
// declarative partitioned table. The result is the partition relation
// names in the same schema as the parent.
func GetPartitionNames(tctx *tcontext.Context, db *BaseConn, schema, table string) (partitions []string, err error) {
	partitions = make([]string, 0)
	const q = `
SELECT cc.relname
FROM pg_catalog.pg_inherits i
JOIN pg_catalog.pg_class p  ON p.oid  = i.inhparent
JOIN pg_catalog.pg_namespace pn ON pn.oid = p.relnamespace
JOIN pg_catalog.pg_class cc ON cc.oid = i.inhrelid
WHERE pn.nspname = $1 AND p.relname = $2
ORDER BY cc.relname`
	err = db.QuerySQL(tctx, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return errors.Trace(err)
		}
		partitions = append(partitions, name)
		return nil
	}, func() {
		partitions = partitions[:0]
	}, q, schema, table)
	return
}

