// Package worker owns the background processing of the wagering module.
package worker

import (
	"context"
	"sync"
	"time"
)

// background owns cancellation and joins all tasks before dependencies close.
// Fx starts it once; task contexts outlive the startup hook's context.
type background struct {
	tasks  []func(context.Context)
	cancel context.CancelFunc
	done   chan struct{}
}

func (b *background) start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done = make(chan struct{})
	var group sync.WaitGroup

	for _, run := range b.tasks {
		group.Go(func() {
			run(ctx)
		})
	}

	go func() {
		group.Wait()
		close(b.done)
	}()

	return nil
}

func (b *background) stop(ctx context.Context) error {
	b.cancel()

	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func pause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
