package oauthutil

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/fs"
)

// RotatingRenew refreshes a rotating token while one or more long-running
// uploads are active.
type RotatingRenew struct {
	name     string
	source   *RotatingTokenSource
	ctx      context.Context
	cancel   context.CancelFunc
	uploads  atomic.Int32
	wake     chan struct{}
	done     chan struct{}
	stopped  chan struct{}
	shutdown sync.Once
}

// NewRotatingRenew starts a background renewer for source.
func NewRotatingRenew(ctx context.Context, name string, source *RotatingTokenSource) *RotatingRenew {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &RotatingRenew{
		name:    name,
		source:  source,
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *RotatingRenew) run() {
	defer close(r.stopped)
	for {
		if r.uploads.Load() == 0 {
			select {
			case <-r.done:
				return
			case <-r.wake:
				continue
			}
		}

		delay := time.Minute
		if r.source != nil {
			if next := r.source.RefreshAfter(); next > 0 {
				delay = next
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-r.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-r.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}

		if r.source == nil || r.uploads.Load() == 0 {
			continue
		}
		if _, _, err := r.source.TokenContext(r.ctx); err != nil {
			fs.Errorf(r.name, "Background rotating token refresher failed: %v", err)
		}
	}
}

// Start marks one upload as active and wakes the renewer to observe its
// current refresh deadline.
func (r *RotatingRenew) Start() {
	if r == nil {
		return
	}
	r.uploads.Add(1)
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Stop marks one upload as complete.
func (r *RotatingRenew) Stop() {
	if r == nil {
		return
	}
	for {
		active := r.uploads.Load()
		if active <= 0 || r.uploads.CompareAndSwap(active, active-1) {
			break
		}
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Shutdown stops the background renewer.
func (r *RotatingRenew) Shutdown() {
	if r == nil {
		return
	}
	r.shutdown.Do(func() {
		r.cancel()
		close(r.done)
	})
	<-r.stopped
}
