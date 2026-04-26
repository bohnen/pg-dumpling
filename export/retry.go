// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"time"

	"github.com/pingcap/tidb/br/pkg/utils"
)

const (
	dumpChunkRetryTime       = 3
	dumpChunkWaitInterval    = 50 * time.Millisecond
	dumpChunkMaxWaitInterval = 200 * time.Millisecond
)

type backOfferResettable interface {
	utils.Backoffer
	Reset()
}

func newRebuildConnBackOffer(shouldRetry bool) backOfferResettable {
	if !shouldRetry {
		return &noopBackoffer{attempt: 1}
	}
	return &dumpChunkBackoffer{
		attempt:      dumpChunkRetryTime,
		delayTime:    dumpChunkWaitInterval,
		maxDelayTime: dumpChunkMaxWaitInterval,
	}
}

type dumpChunkBackoffer struct {
	attempt      int
	delayTime    time.Duration
	maxDelayTime time.Duration
}

func (b *dumpChunkBackoffer) NextBackoff(_ error) time.Duration {
	b.delayTime = 2 * b.delayTime
	b.attempt--
	if b.delayTime > b.maxDelayTime {
		return b.maxDelayTime
	}
	return b.delayTime
}

func (b *dumpChunkBackoffer) Attempt() int { return b.attempt }

func (b *dumpChunkBackoffer) Reset() {
	b.attempt = dumpChunkRetryTime
	b.delayTime = dumpChunkWaitInterval
}

type noopBackoffer struct {
	attempt int
}

func (b *noopBackoffer) NextBackoff(_ error) time.Duration {
	b.attempt--
	return 0
}

func (b *noopBackoffer) Attempt() int { return b.attempt }

func (b *noopBackoffer) Reset() { b.attempt = 1 }
