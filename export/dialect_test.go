// Copyright 2026 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSQLTarget(t *testing.T) {
	cases := []struct {
		in   string
		want SQLTarget
	}{
		{"", TargetMySQL}, // empty defaults to mysql (project's primary use case)
		{"pg", TargetPg},
		{"PG", TargetPg},
		{"postgres", TargetPg},
		{"postgresql", TargetPg},
		{"mysql", TargetMySQL},
		{"MySQL", TargetMySQL},
		{"tidb", TargetTiDB},
		{"  tidb  ", TargetTiDB},
	}
	for _, c := range cases {
		got, err := ParseSQLTarget(c.in)
		require.NoError(t, err, c.in)
		require.Equal(t, c.want, got, c.in)
	}

	_, err := ParseSQLTarget("oracle")
	require.Error(t, err)
}

func TestPgDialect(t *testing.T) {
	d := DialectFor(TargetPg)
	require.Equal(t, "pg", d.Name())
	require.Equal(t, `"foo"`, d.QuoteIdent("foo"))
	require.Equal(t, `"a""b"`, d.QuoteIdent(`a"b`))
	require.Equal(t, `"public"."t"`, d.QuoteQName("public", "t"))
	require.Equal(t, `"t"`, d.QuoteQName("", "t"))
	require.Equal(t, `CREATE SCHEMA IF NOT EXISTS "demo"`, d.CreateDatabaseStmt("demo"))
	require.Equal(t, `'\x48656c6c6f'`, d.BytesLiteral([]byte("Hello")))

	pre := d.Preamble()
	require.Equal(t, []string{
		"SET standard_conforming_strings = on;",
		"SET client_encoding = 'UTF8';",
		"SET search_path = pg_catalog;",
	}, pre)

	require.True(t, d.WantMigrationCasts("csv"))
	require.False(t, d.WantMigrationCasts("sql"))
}

func TestMysqlDialect(t *testing.T) {
	d := DialectFor(TargetMySQL)
	require.Equal(t, "mysql", d.Name())
	require.Equal(t, "`foo`", d.QuoteIdent("foo"))
	require.Equal(t, "`a``b`", d.QuoteIdent("a`b"))
	require.Equal(t, "`public`.`t`", d.QuoteQName("public", "t"))
	require.Equal(t, "`t`", d.QuoteQName("", "t"))
	require.Equal(t, "CREATE DATABASE IF NOT EXISTS `demo`", d.CreateDatabaseStmt("demo"))
	require.Equal(t, "X'48656C6C6F'", d.BytesLiteral([]byte("Hello")))
	require.Equal(t, "X''", d.BytesLiteral(nil))
	require.Equal(t, "X''", d.BytesLiteral([]byte{}))

	pre := d.Preamble()
	require.Contains(t, pre, "/*!40101 SET NAMES utf8mb4 */;")
	require.Contains(t, pre, "SET sql_mode='NO_BACKSLASH_ESCAPES';")

	require.True(t, d.WantMigrationCasts("csv"))
	require.True(t, d.WantMigrationCasts("sql"))
}

func TestTiDBDialectMatchesMySQL(t *testing.T) {
	m := DialectFor(TargetMySQL)
	td := DialectFor(TargetTiDB)
	require.Equal(t, m.QuoteQName("public", "t"), td.QuoteQName("public", "t"))
	require.Equal(t, m.Preamble(), td.Preamble())
	require.Equal(t, "tidb", td.Name())
}
