package worker

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Job represents a single unit of work dispatched from a webhook handler.
// Payload holds the raw, already-validated request body bytes.
// RequestID threads the chi request ID through to worker logs for traceability.
type Job struct {
	RequestID  string
	Payload    []byte
	ReceivedAt time.Time
}

// HandlerFunc is the function signature workers invoke to process a Job.
// Implementations live in the handler/gateway layer — the pool itself is logic-free.
type HandlerFunc func(ctx context.Context, job Job) error

// Pool is a fixed-size worker pool backed by a buffered channel job queue.
// It is safe for concurrent use. Jobs submitted after Shutdown() is called are dropped.
type Pool struct {
	jobs      chan Job
	handler   HandlerFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
	cfg       Config
}

// Config controls pool sizing and retry behaviour.
type Config struct {
	// Workers is the number of goroutines processing jobs concurrently.
	// A sensible default for an I/O-bound service is 2× CPU cores.
	Workers int

	// QueueDepth is the capacity of the buffered job channel.
	// When full, Dispatch() returns ErrQueueFull immediately rather than blocking.
	QueueDepth int

	// JobTimeout is the maximum time a single job may run before its context is cancelled.
	JobTimeout time.Duration
}

// DefaultConfig returns conservative defaults suitable for a low-to-mid traffic webhook service.
func DefaultConfig() Config {
	return Config{
		Workers:    8,
		QueueDepth: 512,
		JobTimeout: 30 * time.Second,
	}
}

// ErrQueueFull is returned by Dispatch when the job channel is at capacity.
var ErrQueueFull = poolError("worker queue is full")

// ErrPoolClosed is returned by Dispatch after Shutdown() has been called.
var ErrPoolClosed = poolError("worker pool is closed")

type poolError string

func (e poolError) Error() string { return string(e) }

// New creates and immediately starts a Pool with the given config and job handler.
// Workers begin consuming from the queue as soon as New returns.
func New(cfg Config, handler HandlerFunc) *Pool {
	p := &Pool{
		jobs:    make(chan Job, cfg.QueueDepth),
		handler: handler,
		closed:  make(chan struct{}),
		cfg:     cfg,
	}

	for range cfg.Workers {
		p.wg.Add(1)
		go p.runWorker()
	}

	slog.Info("worker pool started",
		"workers", cfg.Workers,
		"queue_depth", cfg.QueueDepth,
		"job_timeout", cfg.JobTimeout,
	)

	return p
}

// Dispatch enqueues a job for async processing.
// It returns immediately — callers must not assume the job has been processed.
// Returns ErrQueueFull if the channel is at capacity, ErrPoolClosed after shutdown.
func (p *Pool) Dispatch(job Job) error {
	select {
	case <-p.closed:
		return ErrPoolClosed
	default:
	}

	select {
	case p.jobs <- job:
		return nil
	default:
		slog.Warn("worker queue full, dropping job",
			"request_id", job.RequestID,
		)
		return ErrQueueFull
	}
}

// Shutdown signals workers to stop accepting new jobs, waits for all in-flight
// jobs to complete or the context to expire, whichever comes first.
// Safe to call multiple times — only the first call has effect.
func (p *Pool) Shutdown(ctx context.Context) {
	p.closeOnce.Do(func() {
		close(p.closed) // Signal Dispatch to reject new jobs.
		close(p.jobs)   // Signal workers to drain and exit.
	})

	// Block until all workers finish or the caller's context expires.
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("worker pool drained cleanly")
	case <-ctx.Done():
		slog.Warn("worker pool shutdown timed out, some jobs may be incomplete")
	}
}

// runWorker is the goroutine body for each worker.
// It drains the jobs channel until it is closed, with panic recovery on each job.
func (p *Pool) runWorker() {
	defer p.wg.Done()

	for job := range p.jobs {
		p.processWithRecovery(job)
	}
}

// processWithRecovery executes the handler for one job.
// Any panic is caught, logged with a stack trace, and treated as a failed job
// so the worker goroutine stays alive for subsequent jobs.
func (p *Pool) processWithRecovery(job Job) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in worker",
				"request_id", job.RequestID,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.JobTimeout)
	defer cancel()

	start := time.Now()

	if err := p.handler(ctx, job); err != nil {
		slog.Error("job handler error",
			"request_id", job.RequestID,
			"error", err,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return
	}

	slog.Info("job processed",
		"request_id", job.RequestID,
		"duration_ms", time.Since(start).Milliseconds(),
		"queue_lag_ms", time.Since(job.ReceivedAt).Milliseconds(),
	)
}
