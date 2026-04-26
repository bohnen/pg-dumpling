// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
)

var colTypeRowReceiverMap = map[string]func(SQLDialect) RowReceiverStringer{}

var (
	nullValue         = "NULL"
	quotationMark     = []byte{'\''}
	twoQuotationMarks = []byte{'\'', '\''}
)

// initColTypeRowReceiverMap maps PostgreSQL column type names — as returned
// by pgx via *sql.ColumnType.DatabaseTypeName() — to a row-receiver factory.
// pgx returns the lowercase canonical type name for built-ins (e.g. "int4",
// "text", "bytea", "jsonb") and the type's name without schema for others.
//
// Three receivers cover the entire surface:
//   - SQLTypeNumber: integer / floating / numeric / boolean output unquoted
//     (their textual form is already a valid SQL literal).
//   - SQLTypeBytes:  bytea formatted as PostgreSQL hex literal '\xDEADBEEF'.
//   - SQLTypeString: everything else — wrapped in single quotes with SQL
//     standard quote-doubling escape.
func initColTypeRowReceiverMap() {
	numberTypes := []string{
		"int2", "int4", "int8", "smallint", "integer", "bigint",
		"float4", "float8", "real", "double precision",
		"numeric", "decimal", "money",
		"oid", "xid", "cid", "tid",
		"bool", "boolean",
	}
	binaryTypes := []string{
		"bytea",
	}
	for _, t := range numberTypes {
		colTypeRowReceiverMap[strings.ToUpper(t)] = SQLTypeNumberMaker
		colTypeRowReceiverMap[strings.ToLower(t)] = SQLTypeNumberMaker
		dataTypeInt[strings.ToUpper(t)] = struct{}{}
		dataTypeInt[strings.ToLower(t)] = struct{}{}
	}
	for _, t := range binaryTypes {
		colTypeRowReceiverMap[strings.ToUpper(t)] = SQLTypeBytesMaker
		colTypeRowReceiverMap[strings.ToLower(t)] = SQLTypeBytesMaker
		dataTypeBin[strings.ToUpper(t)] = struct{}{}
		dataTypeBin[strings.ToLower(t)] = struct{}{}
	}
	// All other types (text, varchar, char, date, timestamp(tz), time(tz),
	// interval, uuid, inet, cidr, macaddr, json, jsonb, range/multirange,
	// arrays, enums, domain types) fall through MakeRowReceiver's default to
	// SQLTypeStringMaker.
}

var dataTypeString, dataTypeInt, dataTypeBin = make(map[string]struct{}), make(map[string]struct{}), make(map[string]struct{})

// escapeSQL emits a PostgreSQL string literal body. With
// standard_conforming_strings = on (the modern default), only single quotes
// need escaping and they are doubled.
func escapeSQL(s []byte, bf *bytes.Buffer, _ bool) {
	bf.Write(bytes.ReplaceAll(s, quotationMark, twoQuotationMarks))
}

func escapeCSV(s []byte, bf *bytes.Buffer, _ bool, opt *csvOption) {
	if len(opt.delimiter) > 0 {
		bf.Write(bytes.ReplaceAll(s, opt.delimiter, append(opt.delimiter, opt.delimiter...)))
		return
	}
	bf.Write(s)
}

// SQLTypeStringMaker returns a SQLTypeString
func SQLTypeStringMaker(_ SQLDialect) RowReceiverStringer {
	return &SQLTypeString{}
}

// SQLTypeBytesMaker returns a SQLTypeBytes bound to the given dialect, so
// the resulting binary literals match the target system ('\\xHEX' for
// pg, X'HEX' for mysql/tidb).
func SQLTypeBytesMaker(d SQLDialect) RowReceiverStringer {
	return &SQLTypeBytes{dialect: d}
}

// SQLTypeNumberMaker returns a SQLTypeNumber
func SQLTypeNumberMaker(_ SQLDialect) RowReceiverStringer {
	return &SQLTypeNumber{}
}

// MakeRowReceiver constructs RowReceiverArr from column types. The dialect
// is passed to each receiver factory so binary types can render literals
// in the target-specific form.
func MakeRowReceiver(colTypes []string, dialect SQLDialect) *RowReceiverArr {
	rowReceiverArr := make([]RowReceiverStringer, len(colTypes))
	for i, colTp := range colTypes {
		recMaker, ok := colTypeRowReceiverMap[colTp]
		if !ok {
			recMaker = colTypeRowReceiverMap[strings.ToLower(colTp)]
		}
		if recMaker == nil {
			recMaker = SQLTypeStringMaker
		}
		rowReceiverArr[i] = recMaker(dialect)
	}
	return &RowReceiverArr{
		bound:     false,
		receivers: rowReceiverArr,
	}
}

// RowReceiverArr is the combined RowReceiver array
type RowReceiverArr struct {
	bound     bool
	receivers []RowReceiverStringer
}

// BindAddress implements RowReceiver.BindAddress
func (r *RowReceiverArr) BindAddress(args []any) {
	if r.bound {
		return
	}
	r.bound = true
	for i := range args {
		r.receivers[i].BindAddress(args[i : i+1])
	}
}

// WriteToBuffer implements Stringer.WriteToBuffer
func (r *RowReceiverArr) WriteToBuffer(bf *bytes.Buffer, escapeBackslash bool) {
	bf.WriteByte('(')
	for i, receiver := range r.receivers {
		receiver.WriteToBuffer(bf, escapeBackslash)
		if i != len(r.receivers)-1 {
			bf.WriteByte(',')
		}
	}
	bf.WriteByte(')')
}

// WriteToBufferInCsv implements Stringer.WriteToBufferInCsv
func (r *RowReceiverArr) WriteToBufferInCsv(bf *bytes.Buffer, escapeBackslash bool, opt *csvOption) {
	for i, receiver := range r.receivers {
		receiver.WriteToBufferInCsv(bf, escapeBackslash, opt)
		if i != len(r.receivers)-1 {
			bf.Write(opt.separator)
		}
	}
}

// SQLTypeNumber implements RowReceiverStringer for numeric / boolean columns.
type SQLTypeNumber struct {
	SQLTypeString
}

func (s SQLTypeNumber) WriteToBuffer(bf *bytes.Buffer, _ bool) {
	if s.RawBytes != nil {
		bf.Write(s.RawBytes)
	} else {
		bf.WriteString(nullValue)
	}
}

func (s SQLTypeNumber) WriteToBufferInCsv(bf *bytes.Buffer, _ bool, opt *csvOption) {
	if s.RawBytes != nil {
		bf.Write(s.RawBytes)
	} else {
		bf.WriteString(opt.nullValue)
	}
}

// SQLTypeString covers everything that should be quoted as a string literal.
type SQLTypeString struct {
	sql.RawBytes
}

func (s *SQLTypeString) BindAddress(arg []any) { arg[0] = &s.RawBytes }

func (s *SQLTypeString) WriteToBuffer(bf *bytes.Buffer, escapeBackslash bool) {
	if s.RawBytes != nil {
		bf.Write(quotationMark)
		escapeSQL(s.RawBytes, bf, escapeBackslash)
		bf.Write(quotationMark)
	} else {
		bf.WriteString(nullValue)
	}
}

func (s *SQLTypeString) WriteToBufferInCsv(bf *bytes.Buffer, escapeBackslash bool, opt *csvOption) {
	if s.RawBytes != nil {
		bf.Write(opt.delimiter)
		escapeCSV(s.RawBytes, bf, escapeBackslash, opt)
		bf.Write(opt.delimiter)
	} else {
		bf.WriteString(opt.nullValue)
	}
}

// SQLTypeBytes formats binary columns. The exact literal form is chosen by
// the bound dialect: pg uses '\\xHEX' (PG bytea literal); mysql/tidb use
// X'HEX' (binary literal).
type SQLTypeBytes struct {
	sql.RawBytes
	dialect SQLDialect
}

func (s *SQLTypeBytes) BindAddress(arg []any) { arg[0] = &s.RawBytes }

func (s *SQLTypeBytes) WriteToBuffer(bf *bytes.Buffer, _ bool) {
	if s.RawBytes == nil {
		bf.WriteString(nullValue)
		return
	}
	if s.dialect != nil {
		bf.WriteString(s.dialect.BytesLiteral(s.RawBytes))
		return
	}
	// Fallback to legacy PG behavior when no dialect is bound (e.g.,
	// tests that construct SQLTypeBytes directly without going through
	// MakeRowReceiver).
	fmt.Fprintf(bf, `'\x%x'`, s.RawBytes)
}

func (s *SQLTypeBytes) WriteToBufferInCsv(bf *bytes.Buffer, escapeBackslash bool, opt *csvOption) {
	if s.RawBytes != nil {
		bf.Write(opt.delimiter)
		switch opt.binaryFormat {
		case BinaryFormatHEX:
			fmt.Fprintf(bf, "%x", s.RawBytes)
		case BinaryFormatBase64:
			bf.WriteString(base64.StdEncoding.EncodeToString(s.RawBytes))
		default:
			escapeCSV(s.RawBytes, bf, escapeBackslash, opt)
		}
		bf.Write(opt.delimiter)
	} else {
		bf.WriteString(opt.nullValue)
	}
}
