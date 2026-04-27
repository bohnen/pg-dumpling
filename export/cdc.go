// Copyright 2026 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pingcap/errors"
)

// cdcSlotInfo holds the result of a successful CREATE_REPLICATION_SLOT.
// AWS DMS (and any other logical-decoding consumer) needs SlotName and
// ConsistentPoint to start CDC at the exact point the dump captured.
type cdcSlotInfo struct {
	SlotName        string
	ConsistentPoint string // PG LSN, e.g. "0/16ABCDEF"
	SnapshotName    string // pass-through to SET TRANSACTION SNAPSHOT
	OutputPlugin    string
}

// ValidateCDCName ensures slot names and plugin names contain only the
// characters PostgreSQL accepts as bare identifiers in replication-protocol
// commands. Replication commands do NOT support quoted identifiers, so
// arbitrary names cannot be passed safely.
//
// PG slot name rule: lowercase letters, digits, underscore. Plugin names
// follow the same convention (pgoutput / test_decoding / pglogical_output).
func ValidateCDCName(flag, name string) error {
	if name == "" {
		return errors.Errorf("%s must not be empty", flag)
	}
	if len(name) > 63 {
		return errors.Errorf("%s %q exceeds the 63-character limit", flag, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_':
		default:
			return errors.Errorf("%s %q contains invalid character %q (only [a-z0-9_] allowed in replication-protocol identifiers)", flag, name, string(c))
		}
	}
	return nil
}

// openReplicationConn opens a libpq connection in replication=database mode.
// The caller is responsible for Close()ing the returned conn. The slot's
// exported snapshot remains valid only while THIS conn is alive — workers
// that did SET TRANSACTION SNAPSHOT must do so before this conn is closed.
func openReplicationConn(ctx context.Context, dsn string) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, errors.Annotate(err, "parse replication DSN")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = make(map[string]string)
	}
	cfg.RuntimeParams["replication"] = "database"
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, errors.Annotate(err, "open replication connection")
	}
	return conn, nil
}

// createCDCSlot opens a replication connection and atomically creates a
// logical replication slot with EXPORT_SNAPSHOT. The returned connection
// must be kept open until all worker connections have run SET TRANSACTION
// SNAPSHOT with the returned SnapshotName.
func createCDCSlot(ctx context.Context, dsn, name, plugin string) (*pgconn.PgConn, *cdcSlotInfo, error) {
	if err := ValidateCDCName("--cdc-slot", name); err != nil {
		return nil, nil, err
	}
	if err := ValidateCDCName("--cdc-plugin", plugin); err != nil {
		return nil, nil, err
	}
	conn, err := openReplicationConn(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	stmt := fmt.Sprintf("CREATE_REPLICATION_SLOT %s LOGICAL %s EXPORT_SNAPSHOT", name, plugin)
	results, err := conn.Exec(ctx, stmt).ReadAll()
	if err != nil {
		_ = conn.Close(ctx)
		return nil, nil, errors.Annotatef(err, "CREATE_REPLICATION_SLOT %s", name)
	}
	if len(results) == 0 || len(results[0].Rows) == 0 {
		_ = conn.Close(ctx)
		return nil, nil, errors.Errorf("CREATE_REPLICATION_SLOT returned no rows")
	}
	row := results[0].Rows[0]
	if len(row) < 4 {
		_ = conn.Close(ctx)
		return nil, nil, errors.Errorf("CREATE_REPLICATION_SLOT returned %d columns, expected 4", len(row))
	}
	info := &cdcSlotInfo{
		SlotName:        string(row[0]),
		ConsistentPoint: string(row[1]),
		SnapshotName:    string(row[2]),
		OutputPlugin:    string(row[3]),
	}
	return conn, info, nil
}

// dropCDCSlot opens a fresh replication connection and drops the slot. The
// originating replication connection (the one returned by createCDCSlot)
// MUST already be closed, otherwise PG considers the slot "active" and
// rejects the drop with "replication slot is active for PID ...".
func dropCDCSlot(ctx context.Context, dsn, name string) error {
	if err := ValidateCDCName("--cdc-slot", name); err != nil {
		return err
	}
	conn, err := openReplicationConn(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	stmt := fmt.Sprintf("DROP_REPLICATION_SLOT %s", name)
	if _, err := conn.Exec(ctx, stmt).ReadAll(); err != nil {
		// Not finding the slot is benign; surface anything else.
		if strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return errors.Annotatef(err, "DROP_REPLICATION_SLOT %s", name)
	}
	return nil
}
