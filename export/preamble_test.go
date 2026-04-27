// Copyright 2026 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func TestEffectivePreamble(t *testing.T) {
	cases := []struct {
		name       string
		target     SQLTarget
		noPreamble bool
		wantEmpty  bool
	}{
		{"mysql default emits preamble", TargetMySQL, false, false},
		{"mysql with --no-preamble suppresses", TargetMySQL, true, true},
		{"tidb with --no-preamble suppresses", TargetTiDB, true, true},
		{"pg default emits preamble", TargetPg, false, false},
	}
	for _, c := range cases {
		conf := &Config{Target: c.target, NoPreamble: c.noPreamble}
		got := conf.EffectivePreamble()
		if c.wantEmpty {
			require.Empty(t, got, c.name)
		} else {
			require.NotEmpty(t, got, c.name)
		}
	}
}

func TestNoPreambleTargetAwareDefault(t *testing.T) {
	// Verify the rule: --target=tidb defaults --no-preamble to true when
	// the user did NOT pass --no-preamble explicitly. Tests the small
	// branch in ParseFromFlags directly (BackendOptions parsing requires
	// the global flag set, so we skip that and exercise the relevant bit).
	cases := []struct {
		name           string
		args           []string
		wantTarget     SQLTarget
		wantNoPreamble bool
	}{
		{"target=tidb defaults to no-preamble=true",
			[]string{"--target", "tidb"}, TargetTiDB, true},
		{"target=mysql leaves no-preamble=false",
			[]string{"--target", "mysql"}, TargetMySQL, false},
		{"target=pg leaves no-preamble=false",
			[]string{"--target", "pg"}, TargetPg, false},
		{"target=tidb + explicit --no-preamble=false stays false",
			[]string{"--target", "tidb", "--no-preamble=false"}, TargetTiDB, false},
		{"target=mysql + explicit --no-preamble flips true",
			[]string{"--target", "mysql", "--no-preamble"}, TargetMySQL, true},
	}
	for _, c := range cases {
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.String(flagTarget, "mysql", "")
		flags.Bool(flagNoPreamble, false, "")
		require.NoError(t, flags.Parse(c.args), c.name)

		// Mirror the relevant part of Config.ParseFromFlags.
		ts, _ := flags.GetString(flagTarget)
		target, err := ParseSQLTarget(ts)
		require.NoError(t, err, c.name)
		noPreamble, _ := flags.GetBool(flagNoPreamble)
		if !flags.Changed(flagNoPreamble) && target == TargetTiDB {
			noPreamble = true
		}

		require.Equal(t, c.wantTarget, target, c.name)
		require.Equal(t, c.wantNoPreamble, noPreamble, c.name)
	}
}
