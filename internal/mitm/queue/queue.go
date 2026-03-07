// Package queue implements a bounded request queue with concurrency control,
// matching ZeroGravity's request management: max concurrency, queue depth,
// timeout, and interval between dispatches.
package queue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

type Config struct {
	Enabled     bool
	Concurrency int
	IntervalMS  int
	TimeoutMS   int
	MaxSize     int
}

func DefaultConfig() Config {
	return Config{
		Enabled:     true,
		Concurrency: 2,
		IntervalMS:  0,
		TimeoutMS:   600000,
		MaxSize:     50,
	}
}

type Task func(ctx context.Context) error

type entry struct {
	task   Task
	ctx    context.Context
	result chan error
}

type Queue struct {
	cfg     Config
	entries chan *entry
	sem     chan struct{}
	pending atomic.Int32
	mu      sync.Mutex
	closed  bool
}

func New(cfg Config) *Queue {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 50
	}
	if cfg.TimeoutMS <= 0 {
		cfg.TimeoutMS = 600000
	}

	q := &Queue{
		cfg:     cfg,
		entries: make(chan *entry, cfg.MaxSize),
		sem:     make(chan struct{}, cfg.Concurrency),
	}

	go q.dispatch()
	return q
}

// Submit enqueues a task. Returns an error immediately if the queue is full
// (503) or if the task times out waiting (408).
func (q *Queue) Submit(ctx context.Context, task Task) error {
	if !q.cfg.Enabled {
		return task(ctx)
	}

	if int(q.pending.Load()) >= q.cfg.MaxSize {
		return fmt.Errorf("queue full (%d/%d): service unavailable", q.pending.Load(), q.cfg.MaxSize)
	}

	timeout := time.Duration(q.cfg.TimeoutMS) * time.Millisecond
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	e := &entry{
		task:   task,
		ctx:    timeoutCtx,
		result: make(chan error, 1),
	}

	q.pending.Add(1)
	select {
	case q.entries <- e:
	default:
		q.pending.Add(-1)
		return fmt.Errorf("queue full: service unavailable")
	}

	select {
	case err := <-e.result:
		return err
	case <-timeoutCtx.Done():
		q.pending.Add(-1)
		return fmt.Errorf("request timeout after %dms", q.cfg.TimeoutMS)
	}
}

func (q *Queue) dispatch() {
	interval := time.Duration(q.cfg.IntervalMS) * time.Millisecond

	for e := range q.entries {
		if e.ctx.Err() != nil {
			q.pending.Add(-1)
			e.result <- e.ctx.Err()
			continue
		}

		q.sem <- struct{}{}

		go func(e *entry) {
			defer func() {
				<-q.sem
				q.pending.Add(-1)
			}()

			err := e.task(e.ctx)
			e.result <- err

			if interval > 0 {
				time.Sleep(interval)
			}
		}(e)
	}
}

// Pending returns the number of pending tasks.
func (q *Queue) Pending() int {
	return int(q.pending.Load())
}

// Close drains the queue.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.entries)
		log.Info("request queue: closed")
	}
}
