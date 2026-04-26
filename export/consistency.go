// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"context"
	"database/sql"

	"github.com/pingcap/errors"
	tcontext "github.com/tadapin/pg-dumpling/context"
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
}

// ConsistencyNone runs without acquiring any consistency token.
type ConsistencyNone struct{}

// Setup implements ConsistencyController.
func (*ConsistencyNone) Setup(_ *tcontext.Context) error { return nil }

// TearDown implements ConsistencyController.
func (*ConsistencyNone) TearDown(_ context.Context) error { return nil }

// PingContext implements ConsistencyController.
func (*ConsistencyNone) PingContext(_ context.Context) error { return nil }

// ConsistencySnapshot opens a REPEATABLE READ READ ONLY transaction on a
// dedicated connection and exports its snapshot token via pg_export_snapshot.
// Worker connections later run "SET TRANSACTION SNAPSHOT" with that token to
// observe the same MVCC view. The dedicated connection is held until TearDown
// to keep the snapshot alive.
type ConsistencySnapshot struct {
	conn         *sql.Conn
	conf         *Config
	snapshotName string
}

// Setup begins the snapshot transaction and captures the token.
func (c *ConsistencySnapshot) Setup(tctx *tcontext.Context) error {
	if _, err := c.conn.ExecContext(tctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return errors.Annotate(err, "begin snapshot transaction")
	}
	if c.conf.Snapshot != "" {
		if _, err := c.conn.ExecContext(tctx, "SET TRANSACTION SNAPSHOT '"+escapeStringLiteral(c.conf.Snapshot)+"'"); err != nil {
			return errors.Annotate(err, "set transaction snapshot")
		}
		c.snapshotName = c.conf.Snapshot
		return nil
	}
	row := c.conn.QueryRowContext(tctx, "SELECT pg_export_snapshot()")
	var token string
	if err := row.Scan(&token); err != nil {
		return errors.Annotate(err, "pg_export_snapshot")
	}
	c.snapshotName = token
	c.conf.Snapshot = token
	return nil
}

// SnapshotName returns the exported snapshot token.
func (c *ConsistencySnapshot) SnapshotName() string { return c.snapshotName }

// TearDown closes the snapshot transaction.
func (c *ConsistencySnapshot) TearDown(ctx context.Context) error {
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
