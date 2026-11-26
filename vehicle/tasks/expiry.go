package tasks

import (
	"context"
	"time"
)

// ExpiryWorker periodically marks overdue pending_reply tasks as expired.
// Candidate queries and dispatch still enforce expires_at > now synchronously so
// scan lag can never allow resuming an expired task.
type ExpiryWorker struct {
	Store    Store
	Interval time.Duration
	Now      func() time.Time
	OnError  func(error)
}

func (w *ExpiryWorker) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	nowFn := w.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := w.Store.ExpirePending(ctx, nowFn()); err != nil && w.OnError != nil {
				w.OnError(err)
			}
		}
	}
}
