// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"bytes"
	"database/sql"
	"time"

	"github.com/pingcap/tidb/br/pkg/storage"
	tcontext "github.com/tadapin/pg-dumpling/context"
)

type globalMetadata struct {
	tctx            *tcontext.Context
	buffer          bytes.Buffer
	afterConnBuffer bytes.Buffer
	snapshot        string

	storage storage.ExternalStorage
}

const (
	metadataPath       = "metadata"
	metadataTimeLayout = time.DateTime
)

func newGlobalMetadata(tctx *tcontext.Context, s storage.ExternalStorage, snapshot string) *globalMetadata {
	return &globalMetadata{
		tctx:     tctx,
		storage:  s,
		buffer:   bytes.Buffer{},
		snapshot: snapshot,
	}
}

func (m globalMetadata) String() string {
	return m.buffer.String()
}

func (m *globalMetadata) recordStartTime(t time.Time) {
	m.buffer.WriteString("Started dump at: " + t.Format(metadataTimeLayout) + "\n")
}

func (m *globalMetadata) recordFinishTime(t time.Time) {
	m.buffer.Write(m.afterConnBuffer.Bytes())
	m.buffer.WriteString("Finished dump at: " + t.Format(metadataTimeLayout) + "\n")
}

// recordGlobalMetaData records server-side global metadata. Currently a
// no-op for PostgreSQL; a later step will add pg_current_wal_lsn().
func (m *globalMetadata) recordGlobalMetaData(_ *sql.Conn, _ bool) error {
	return nil
}

func (m *globalMetadata) writeGlobalMetaData() error {
	// keep consistent with mydumper. Never compress metadata
	fileWriter, tearDown, err := buildFileWriter(m.tctx, m.storage, metadataPath, storage.NoCompression)
	if err != nil {
		return err
	}
	err = write(m.tctx, fileWriter, m.String())
	tearDownErr := tearDown(m.tctx)
	if err == nil {
		return tearDownErr
	}
	return err
}

