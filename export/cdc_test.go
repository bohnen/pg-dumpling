// Copyright 2026 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateCDCName(t *testing.T) {
	good := []string{
		"mycdc",
		"a",
		"slot_1",
		"pgoutput",
		"pglogical_output",
		"test_decoding",
		"abcdefghijklmnopqrstuvwxyz_0123456789_aaaaaaaaaaaaaaaaaaaaaaaaa", // 63 chars
	}
	for _, n := range good {
		require.NoError(t, ValidateCDCName("--cdc-slot", n), n)
	}

	bad := []struct {
		in   string
		want string
	}{
		{"", "must not be empty"},
		{"MyCDC", "invalid character"},        // uppercase
		{"my-cdc", "invalid character"},       // hyphen
		{"my.cdc", "invalid character"},       // dot
		{"my cdc", "invalid character"},       // space
		{"slot;DROP TABLE", "invalid character"}, // semicolon — injection guard
		{"slot\"injected", "invalid character"},
		{"verylongnameverylongnameverylongnameverylongnameverylongname1234", "exceeds the 63-character limit"},
	}
	for _, c := range bad {
		err := ValidateCDCName("--cdc-slot", c.in)
		require.Error(t, err, c.in)
		require.Contains(t, err.Error(), c.want, c.in)
	}
}
