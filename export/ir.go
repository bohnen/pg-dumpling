// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"bytes"
	"database/sql"
	"strings"

	"github.com/pingcap/errors"
	tcontext "github.com/tadapin/pg-dumpling/context"
)

// TableDataIR is table data intermediate representation.
// A table may be split into multiple TableDataIRs.
type TableDataIR interface {
	Start(*tcontext.Context, *sql.Conn) error
	Rows() SQLRowIter
	Close() error
	RawRows() *sql.Rows
}

// TableMeta contains the meta information of a table.
type TableMeta interface {
	DatabaseName() string
	TableName() string
	ColumnCount() uint
	ColumnTypes() []string
	ColumnNames() []string
	SelectedField() string
	SelectColumns() string
	SelectedLen() int
	SpecialComments() StringIter
	ShowCreateTable() string
	ShowCreateView() string
	AvgRowLength() uint64
	HasImplicitRowID() bool
}

// SQLRowIter is the iterator on a collection of sql.Row.
type SQLRowIter interface {
	Decode(RowReceiver) error
	Next()
	Error() error
	HasNext() bool
	// release SQLRowIter
	Close() error
}

// RowReceiverStringer is a combined interface of RowReceiver and Stringer
type RowReceiverStringer interface {
	RowReceiver
	Stringer
}

// Stringer is an interface which represents sql types that support writing to buffer in sql/csv type
type Stringer interface {
	WriteToBuffer(*bytes.Buffer, bool)
	WriteToBufferInCsv(*bytes.Buffer, bool, *csvOption)
}

// RowReceiver is an interface which represents sql types that support bind address for *sql.Rows
type RowReceiver interface {
	BindAddress([]any)
}

func decodeFromRows(rows *sql.Rows, args []any, row RowReceiver) error {
	row.BindAddress(args)
	if err := rows.Scan(args...); err != nil {
		rows.Close()
		return errors.Trace(err)
	}
	return nil
}

// StringIter is the iterator on a collection of strings.
type StringIter interface {
	Next() string
	HasNext() bool
}

// MetaIR is the interface that wraps database/table/view's metadata
type MetaIR interface {
	SpecialComments() StringIter
	TargetName() string
	MetaSQL() string
}

func setTableMetaFromRows(rows *sql.Rows, conf *Config) (TableMeta, error) {
	tps, err := rows.ColumnTypes()
	if err != nil {
		return nil, errors.Trace(err)
	}
	nms, err := rows.Columns()
	if err != nil {
		return nil, errors.Trace(err)
	}
	dialect := conf.Dialect()
	exprs := make([]string, len(nms))
	cols := make([]string, len(nms))
	for i, n := range nms {
		exprs[i] = pgQuoteIdent(n)
		cols[i] = dialect.QuoteIdent(n)
	}
	return &tableMeta{
		colTypes:      tps,
		selectedField: strings.Join(exprs, ","),
		selectColumns: strings.Join(cols, ","),
		selectedLen:   len(nms),
		specCmts:      conf.EffectivePreamble(),
	}, nil
}
