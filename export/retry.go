// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"context"
	"time"

	"go.uber.org/multierr"
)

const (
	dumpChunkRetryTime       = 3
	dumpChunkWaitInterval    = 50 * time.Millisecond
	dumpChunkMaxWaitInterval = 200 * time.Millisecond
)

// Backoffer schedules retry attempts. Inlined from
// github.com/pingcap/tidb/br/pkg/utils.Backoffer.
type Backoffer interface {
	// NextBackoff returns the duration to wait before the next attempt.
	NextBackoff(err error) time.Duration
	// Attempt returns how many attempts remain (zero or negative -> stop).
	Attempt() int
}

// WithRetry runs fn until it returns nil, the Backoffer runs out of attempts,
// or ctx is cancelled. The error from each failing attempt is accumulated
// into a multierr so callers can inspect every cause.
//
// Inlined from github.com/pingcap/tidb/br/pkg/WithRetry minus
// generic / SampleLogger flavour we don't use.
func WithRetry(ctx context.Context, fn func() error, b Backoffer) error {
	var allErrors error
	for b.Attempt() > 0 {
		err := fn()
		if err == nil {
			return nil
		}
		allErrors = multierr.Append(allErrors, err)
		select {
		case <-ctx.Done():
			return allErrors
		case <-time.After(b.NextBackoff(err)):
		}
	}
	return allErrors
}

type backOfferResettable interface {
	Backoffer
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
