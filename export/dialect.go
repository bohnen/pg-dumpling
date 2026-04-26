// Copyright 2026 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"fmt"
	"strings"

	"github.com/pingcap/errors"
)

// SQLDialect describes the flavor of SQL the dumper should emit when
// FileType=sql. The CSV path uses CSVDialect (Snowflake/Redshift/BigQuery)
// to control loader-side quirks; SQLDialect instead controls the SQL
// statements themselves (identifier quoting, preamble, byte literal form,
// etc.) so that --filetype=sql output can target either Postgres
// (round-trip) or MySQL/TiDB (migration).
type SQLDialect interface {
	// Name returns the canonical name for diagnostics/logging.
	Name() string

	// QuoteIdent quotes a single identifier (column / schema / table name).
	QuoteIdent(name string) string

	// QuoteQName quotes a qualified "schema"."name". When schema is empty,
	// the bare quoted name is returned.
	QuoteQName(schema, name string) string

	// Preamble is the slice of SET-style statements emitted at the top of
	// every data file. The dialect is responsible for terminating each line
	// with a semicolon if needed.
	Preamble() []string

	// CreateDatabaseStmt builds the statement written into
	// <schema>-schema-create.sql.
	CreateDatabaseStmt(name string) string

	// BytesLiteral renders a binary blob as a SQL literal acceptable by
	// the dialect (e.g. PG's '\x...' or MySQL's X'...').
	BytesLiteral(b []byte) string

	// WantMigrationCasts reports whether the dumper should wrap PG-specific
	// columns in the to_jsonb / to_char / encode casts produced by
	// pgMigrationCast(). For target=mysql/tidb we want them in BOTH csv and
	// sql modes; for target=pg we only want them in csv mode.
	WantMigrationCasts(filetype string) bool
}

// SQLTarget identifies the target system for --filetype=sql output.
type SQLTarget int

const (
	// TargetPg keeps the historical PG-native SQL output (suitable for
	// `psql -f` round-trip).
	TargetPg SQLTarget = iota
	// TargetMySQL emits MySQL-compatible SQL (backtick quoting, MySQL
	// preamble, X'...' bytea literals).
	TargetMySQL
	// TargetTiDB is currently identical to TargetMySQL; reserved for future
	// divergence.
	TargetTiDB
)

// String returns the canonical CLI value for the target.
func (t SQLTarget) String() string {
	switch t {
	case TargetPg:
		return "pg"
	case TargetMySQL:
		return "mysql"
	case TargetTiDB:
		return "tidb"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// ParseSQLTarget maps the --target flag string to a SQLTarget. Empty string
// parses to the project default (TargetMySQL — the primary intent of
// pg-dumpling is PG → MySQL/TiDB migration; round-trip to PG is opt-in via
// --target=pg).
func ParseSQLTarget(s string) (SQLTarget, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "mysql":
		return TargetMySQL, nil
	case "tidb":
		return TargetTiDB, nil
	case "pg", "postgres", "postgresql":
		return TargetPg, nil
	default:
		return TargetPg, errors.Errorf("unknown --target value %q (want pg|mysql|tidb)", s)
	}
}

// DialectFor returns the SQLDialect implementation bound to the target.
func DialectFor(t SQLTarget) SQLDialect {
	switch t {
	case TargetMySQL, TargetTiDB:
		return mysqlDialect{name: t.String()}
	default:
		return pgDialect{}
	}
}

// Dialect returns the SQLDialect for this Config's target.
func (conf *Config) Dialect() SQLDialect { return DialectFor(conf.Target) }

// pgDialect emits the historical PG-native SQL output. This is what every
// data file produced before v0.6.0 looked like, so we keep it 1:1 with the
// previous pgQuoteIdent / pgFilePreamble / SQLTypeBytes behavior.
type pgDialect struct{}

func (pgDialect) Name() string { return "pg" }

func (pgDialect) QuoteIdent(name string) string {
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

func (d pgDialect) QuoteQName(schema, name string) string {
	if schema == "" {
		return d.QuoteIdent(name)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(name)
}

func (pgDialect) Preamble() []string {
	// - standard_conforming_strings = on: '...' literals don't interpret
	//   backslash escapes (matches escapeSQL).
	// - client_encoding = UTF8: matches what the dumper requested.
	// - search_path = pg_catalog: defends against malicious user-defined
	//   functions/operators shadowing built-ins during reload.
	return []string{
		"SET standard_conforming_strings = on;",
		"SET client_encoding = 'UTF8';",
		"SET search_path = pg_catalog;",
	}
}

func (d pgDialect) CreateDatabaseStmt(name string) string {
	return fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", d.QuoteIdent(name))
}

func (pgDialect) BytesLiteral(b []byte) string {
	return fmt.Sprintf(`'\x%x'`, b)
}

func (pgDialect) WantMigrationCasts(filetype string) bool {
	// PG round-trip: only CSV needs the casts (so that bytea / arrays /
	// timestamptz come out as ASCII-safe text). SQL mode keeps the native
	// PG literal form.
	return filetype == "csv"
}

// mysqlDialect emits MySQL/TiDB-compatible SQL. See worklog/06.
type mysqlDialect struct{ name string }

func (d mysqlDialect) Name() string {
	if d.name == "" {
		return "mysql"
	}
	return d.name
}

func (mysqlDialect) QuoteIdent(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 2)
	b.WriteByte('`')
	for i := 0; i < len(name); i++ {
		if name[i] == '`' {
			b.WriteByte('`')
		}
		b.WriteByte(name[i])
	}
	b.WriteByte('`')
	return b.String()
}

func (d mysqlDialect) QuoteQName(schema, name string) string {
	if schema == "" {
		return d.QuoteIdent(name)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(name)
}

func (mysqlDialect) Preamble() []string {
	// SQL standard quote-doubling stays on via NO_BACKSLASH_ESCAPES, so
	// the existing escapeSQL output is consumable as-is.
	return []string{
		"/*!40101 SET NAMES utf8mb4 */;",
		"/*!40014 SET FOREIGN_KEY_CHECKS=0 */;",
		"/*!40014 SET UNIQUE_CHECKS=0 */;",
		"SET sql_mode='NO_BACKSLASH_ESCAPES';",
	}
}

func (d mysqlDialect) CreateDatabaseStmt(name string) string {
	return fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", d.QuoteIdent(name))
}

func (mysqlDialect) BytesLiteral(b []byte) string {
	if len(b) == 0 {
		return "X''"
	}
	return fmt.Sprintf("X'%X'", b)
}

func (mysqlDialect) WantMigrationCasts(_ string) bool {
	// MySQL/TiDB target: always run the PG-side casts so that timestamptz,
	// arrays, intervals etc. come back as MySQL-friendly text. bytea is
	// special-cased inside pgMigrationCast (no encode for SQL mode — we let
	// SQLTypeBytes write X'...' directly).
	return true
}
