package worker_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

// newPool is a test helper to spin up a pool and guarantee cleanup.
func newPool(t *testing.T, cfg worker.Config, fn worker.HandlerFunc) *worker.Pool {
	t.Helper()
	p := worker.New(cfg, fn)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.Shutdown(ctx)
	})
	return p
}

func TestPool_ProcessesJobs(t *testing.T) {
	var processed atomic.Int64

	cfg := worker.DefaultConfig()
	p := newPool(t, cfg, func(_ context.Context, _ worker.Job) error {
		processed.Add(1)
		return nil
	})

	const total = 20
	for i := range total {
		job := worker.Job{RequestID: fmt.Sprintf("req-%d", i), ReceivedAt: time.Now()}
		if err := p.Dispatch(job); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}

	// Allow workers time to drain.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p.Shutdown(ctx)

	if got := processed.Load(); got != total {
		t.Errorf("expected %d processed, got %d", total, got)
	}
}

func TestPool_ReturnsErrQueueFull(t *testing.T) {
	// A handler that blocks until the test releases it — keeps the queue full.
	release := make(chan struct{})
	cfg := worker.Config{Workers: 1, QueueDepth: 1, JobTimeout: 5 * time.Second}

	p := newPool(t, cfg, func(_ context.Context, _ worker.Job) error {
		<-release
		return nil
	})

	// Fill the single worker and the single queue slot.
	_ = p.Dispatch(worker.Job{ReceivedAt: time.Now()})
	_ = p.Dispatch(worker.Job{ReceivedAt: time.Now()})

	// This third dispatch must be rejected immediately.
	err := p.Dispatch(worker.Job{ReceivedAt: time.Now()})
	if !errors.Is(err, worker.ErrQueueFull) {
		t.Errorf("expected ErrQueueFull, got %v", err)
	}

	close(release)
}

func TestPool_ReturnsErrPoolClosed(t *testing.T) {
	cfg := worker.Config{Workers: 1, QueueDepth: 4, JobTimeout: 5 * time.Second}
	p := worker.New(cfg, func(_ context.Context, _ worker.Job) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Shutdown(ctx)

	err := p.Dispatch(worker.Job{ReceivedAt: time.Now()})
	if !errors.Is(err, worker.ErrPoolClosed) {
		t.Errorf("expected ErrPoolClosed, got %v", err)
	}
}

func TestPool_PanicInWorkerDoesNotKillPool(t *testing.T) {
	var processed atomic.Int64
	panicOnce := atomic.Bool{}
	panicOnce.Store(true)

	cfg := worker.Config{Workers: 2, QueueDepth: 16, JobTimeout: 5 * time.Second}
	p := newPool(t, cfg, func(_ context.Context, _ worker.Job) error {
		if panicOnce.CompareAndSwap(true, false) {
			panic("intentional test panic")
		}
		processed.Add(1)
		return nil
	})

	// First job panics; subsequent jobs must still be processed.
	for range 5 {
		_ = p.Dispatch(worker.Job{ReceivedAt: time.Now()})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p.Shutdown(ctx)

	// 1 panic + 5 dispatched = 4 successfully processed.
	if got := processed.Load(); got != 4 {
		t.Errorf("expected 4 processed after panic, got %d", got)
	}
}
