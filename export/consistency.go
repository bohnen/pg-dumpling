// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pingcap/errors"
	tcontext "github.com/tadapin/pg-dumpling/context"
	"go.uber.org/zap"
)

const (
	// ConsistencyTypeAuto resolves to Snapshot for PostgreSQL.
	ConsistencyTypeAuto = "auto"
	// ConsistencyTypeSnapshot uses a REPEATABLE READ READ ONLY transaction with
	// pg_export_snapshot() so worker connections can SET TRANSACTION SNAPSHOT
	// to share the same MVCC view.
	ConsistencyTypeSnapshot = "snapshot"
	// ConsistencyTypeNone runs without any consistency setup.
	ConsistencyTypeNone = "none"
)

// NewConsistencyController returns a new consistency controller for PostgreSQL.
func NewConsistencyController(ctx context.Context, conf *Config, session *sql.DB) (ConsistencyController, error) {
	conn, err := session.Conn(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	switch conf.Consistency {
	case ConsistencyTypeAuto, ConsistencyTypeSnapshot:
		return &ConsistencySnapshot{conn: conn, conf: conf}, nil
	case ConsistencyTypeNone:
		_ = conn.Close()
		return &ConsistencyNone{}, nil
	default:
		_ = conn.Close()
		return nil, errors.Errorf("invalid consistency option %s", conf.Consistency)
	}
}

// ConsistencyController controls the consistency of the export.
type ConsistencyController interface {
	Setup(*tcontext.Context) error
	TearDown(context.Context) error
	PingContext(context.Context) error
	// OnFailure is invoked when the surrounding dump returned an error.
	// Implementations clean up any state that should not outlive a failed
	// run (e.g., logical replication slots that would otherwise hold WAL
	// indefinitely). It must be safe to call after TearDown.
	OnFailure(context.Context) error
}

// ConsistencyNone runs without acquiring any consistency token.
type ConsistencyNone struct{}

// Setup implements ConsistencyController.
func (*ConsistencyNone) Setup(_ *tcontext.Context) error { return nil }

// TearDown implements ConsistencyController.
func (*ConsistencyNone) TearDown(_ context.Context) error { return nil }

// PingContext implements ConsistencyController.
func (*ConsistencyNone) PingContext(_ context.Context) error { return nil }

// OnFailure implements ConsistencyController. No-op for ConsistencyNone.
func (*ConsistencyNone) OnFailure(_ context.Context) error { return nil }

// ConsistencySnapshot opens a REPEATABLE READ READ ONLY transaction on a
// dedicated connection and exports its snapshot token via pg_export_snapshot.
// Worker connections later run "SET TRANSACTION SNAPSHOT" with that token to
// observe the same MVCC view. The dedicated connection is held until TearDown
// to keep the snapshot alive.
type ConsistencySnapshot struct {
	conn         *sql.Conn
	conf         *Config
	snapshotName string

	// cdcConn is the libpq replication-mode connection that issued
	// CREATE_REPLICATION_SLOT ... EXPORT_SNAPSHOT. Its lifetime governs
	// the validity of the exported snapshot, so we keep it open until
	// TearDown. nil when --cdc-slot was not requested.
	cdcConn *pgconn.PgConn
	cdcSlot *cdcSlotInfo
}

// Setup begins the snapshot transaction and captures the token. The token is
// also published via SetPgSnapshotToken so worker connections can adopt it.
//
// When --cdc-slot is set, Setup additionally opens a replication-protocol
// connection and issues CREATE_REPLICATION_SLOT ... EXPORT_SNAPSHOT. The
// returned snapshot_name supersedes pg_export_snapshot(), giving CDC
// consumers (AWS DMS et al.) a slot whose consistent_point matches the
// dump's MVCC view exactly.
func (c *ConsistencySnapshot) Setup(tctx *tcontext.Context) error {
	if c.conf.CDCSlot != "" && c.conf.Snapshot == "" {
		conn, slot, err := createCDCSlot(tctx, c.conf.GetDSN(""), c.conf.CDCSlot, c.conf.CDCPlugin)
		if err != nil {
			return errors.Trace(err)
		}
		c.cdcConn = conn
		c.cdcSlot = slot
		c.conf.cdcSlotInfo = slot
		c.conf.Snapshot = slot.SnapshotName
		tctx.L().Info("created CDC replication slot",
			zap.String("slot", slot.SlotName),
			zap.String("plugin", slot.OutputPlugin),
			zap.String("consistent_point", slot.ConsistentPoint),
			zap.String("snapshot_name", slot.SnapshotName))
	}

	if _, err := c.conn.ExecContext(tctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return errors.Annotate(err, "begin snapshot transaction")
	}
	if c.conf.Snapshot != "" {
		if _, err := c.conn.ExecContext(tctx, "SET TRANSACTION SNAPSHOT '"+escapeStringLiteral(c.conf.Snapshot)+"'"); err != nil {
			return errors.Annotate(err, "set transaction snapshot")
		}
		c.snapshotName = c.conf.Snapshot
		SetPgSnapshotToken(c.snapshotName)
		return nil
	}
	row := c.conn.QueryRowContext(tctx, "SELECT pg_export_snapshot()")
	var token string
	if err := row.Scan(&token); err != nil {
		return errors.Annotate(err, "pg_export_snapshot")
	}
	c.snapshotName = token
	c.conf.Snapshot = token
	SetPgSnapshotToken(token)
	return nil
}

// SnapshotName returns the exported snapshot token.
func (c *ConsistencySnapshot) SnapshotName() string { return c.snapshotName }

// TearDown closes the snapshot transaction. The CDC replication
// connection (if any) is also closed; PG keeps the slot itself alive so a
// downstream CDC consumer can attach later.
func (c *ConsistencySnapshot) TearDown(ctx context.Context) error {
	if c.cdcConn != nil {
		_ = c.cdcConn.Close(ctx)
		c.cdcConn = nil
	}
	if c.conn == nil {
		return nil
	}
	defer func() {
		_ = c.conn.Close()
		c.conn = nil
	}()
	if _, err := c.conn.ExecContext(ctx, "COMMIT"); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// OnFailure drops the CDC replication slot when --cdc-cleanup-on-failure is
// set, so a failed dump does not leave the slot behind retaining WAL.
func (c *ConsistencySnapshot) OnFailure(ctx context.Context) error {
	if c.cdcSlot == nil || !c.conf.CDCCleanupOnFailure {
		return nil
	}
	// The originating replication conn must already be closed before drop;
	// TearDown has been called by the time OnFailure runs.
	if c.cdcConn != nil {
		_ = c.cdcConn.Close(ctx)
		c.cdcConn = nil
	}
	return dropCDCSlot(ctx, c.conf.GetDSN(""), c.cdcSlot.SlotName)
}

// PingContext keeps the snapshot connection alive.
func (c *ConsistencySnapshot) PingContext(ctx context.Context) error {
	if c.conn == nil {
		return errors.New("snapshot connection has already been closed")
	}
	return c.conn.PingContext(ctx)
}

// escapeStringLiteral doubles single quotes for SQL standard string literals.
func escapeStringLiteral(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}
