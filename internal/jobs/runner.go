// Package jobs runs work that must outlive the request that asked for it.
//
// The 2026-10-09 review's theme 4 is that long work was bound to HTTP requests:
// a scan, an enrich pass and an OCR job each ran synchronously inside a handler
// under r.Context(), so a client that disconnected - or an operator's 15-second
// UI timeout - cancelled work that had nothing to do with that client. Nothing
// was serialised either, so two scans of one library raced, N players on one
// subtitle track started N OCR passes, and the results of the loser were thrown
// away (W-2, L-13, A-B7).
//
// Three properties fix that, and this package exists to hold them in one place:
//
//   - **Detached.** A job runs on a context of its own, cancelled when the
//     runner stops, never when a request ends.
//   - **Deduplicated.** One job per key at a time. A second request for the same
//     key is told which job already holds it rather than starting another.
//   - **Bounded.** A fixed number of jobs run at once; the rest wait in a queue,
//     so a caller looping on an endpoint cannot start unbounded work.
//
// What it deliberately is not: a durable queue. Jobs live in memory and are lost
// on restart, which is right for this server - every job here is derived work
// that a rescan or a retry reproduces - and it keeps the package small enough to
// read in one sitting.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrQueueFull is returned when the runner already holds as many waiting jobs as
// it will accept.
var ErrQueueFull = errors.New("jobs: the queue is full")

// State is where a job has got to.
type State string

const (
	// StateQueued means the job is waiting for a worker.
	StateQueued State = "queued"
	// StateRunning means a worker is executing it.
	StateRunning State = "running"
	// StateDone means it finished, successfully or not.
	StateDone State = "done"
)

// Result is what a job produced, or why it failed.
type Result struct {
	// Value is whatever the work returned. Its type is the caller's business;
	// the runner never looks at it.
	Value any
	// Err is the failure, if any.
	Err error
}

// Job is a handle on submitted work.
type Job struct {
	// ID identifies this job for as long as it is remembered.
	ID string
	// Key is the resource the job is about. Two submissions with one key are the
	// same job while it is queued or running.
	Key string

	done chan struct{}
	work func(context.Context) (any, error)

	mu      sync.Mutex
	state   State
	started time.Time
	ended   time.Time
	result  Result
}

// State reports where the job has got to.
func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

// StartedAt and EndedAt report the job's timing. EndedAt is zero until it ends.
func (j *Job) StartedAt() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.started
}

// EndedAt reports when the job finished, or the zero time while it is running.
func (j *Job) EndedAt() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.ended
}

// Done returns a channel closed when the job ends.
func (j *Job) Done() <-chan struct{} { return j.done }

// Wait blocks until the job ends and returns its result.
//
// It waits on the job, not on any caller's context: a caller that gives up does
// not stop the work, which is the point of the package.
func (j *Job) Wait() Result {
	<-j.done
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result
}

// WaitContext waits for the job to end, or for ctx to be done.
//
// It reports which happened, so a caller can tell a finished job from one it
// stopped waiting for. The job itself is unaffected: nothing a caller does
// cancels work on the runner.
func (j *Job) WaitContext(ctx context.Context) (Result, bool) {
	select {
	case <-j.done:
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.result, true
	case <-ctx.Done():
		return Result{}, false
	}
}

// Runner executes jobs. The zero value is not usable; call New.
type Runner struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	byID     map[string]*Job
	queue    chan *Job
	baseCtx  context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	nextID   int
	capacity int
	closed   bool
}

// Config configures a Runner.
type Config struct {
	// Workers is how many jobs may run at once. Zero means a small default.
	Workers int
	// Queue is how many jobs may wait. Zero means a small default, and a
	// negative value means unbounded - which is only sensible when the callers
	// are trusted, because it is the queue that a loop of requests fills.
	Queue int
}

// defaultWorkers is deliberately small. Every job here is ffmpeg, a filesystem
// walk or an HTTP provider call, and a household server wants two of those at
// once rather than sixteen.
const defaultWorkers = 2

// defaultQueue bounds the waiting list. It is large enough for a burst of
// per-library scans and small enough that a loop cannot grow it without limit.
const defaultQueue = 64

// New builds a Runner and starts its workers.
func New(parent context.Context, cfg Config, logger Logger) *Runner {
	workers := cfg.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	queueSize := cfg.Queue
	if queueSize == 0 {
		queueSize = defaultQueue
	}
	if logger == nil {
		logger = discardLogger{}
	}

	// A job's context descends from the runner's, not from any request's, so
	// cancelling a request cannot cancel work - and stopping the runner does.
	baseCtx, cancel := context.WithCancel(parent)

	runner := &Runner{
		jobs:     make(map[string]*Job),
		byID:     make(map[string]*Job),
		baseCtx:  baseCtx,
		cancel:   cancel,
		capacity: queueSize,
	}
	if cfg.Queue < 0 {
		// An unbounded queue: a caller may know its own submissions are bounded.
		// The default is bounded, because a queue a request loop can grow is the
		// thing the limit exists for.
		runner.queue = make(chan *Job, 1<<20)
		runner.capacity = 0
	} else {
		runner.queue = make(chan *Job, queueSize)
	}

	for i := 0; i < workers; i++ {
		runner.wg.Add(1)
		go runner.work(logger)
	}
	return runner
}

// Submit starts or joins a job for a key.
//
// One key has one job at a time. A second submission for a key that is queued or
// running returns that job and reports false, so a caller can tell "I started
// this" from "this was already happening" - which is the difference between
// answering 202 with new work and 200 with the work in flight.
//
// The work receives a context that is cancelled when the runner stops, never
// when a request ends.
func (r *Runner) Submit(key string, work func(context.Context) (any, error)) (*Job, bool, error) {
	if work == nil {
		return nil, false, errors.New("jobs: no work to run")
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, false, errors.New("jobs: the runner is closed")
	}
	if existing, ok := r.jobs[key]; ok && existing.State() != StateDone {
		r.mu.Unlock()
		return existing, false, nil
	}

	job := &Job{
		Key:   key,
		work:  work,
		state: StateQueued,
		done:  make(chan struct{}),
	}
	r.nextID++
	job.ID = fmt.Sprintf("job-%d", r.nextID)

	// The job is registered before it is queued, so a submission that races this
	// one joins it rather than starting a second.
	r.jobs[key] = job
	r.byID[job.ID] = job

	if r.capacity > 0 && len(r.queue) >= r.capacity {
		// Unregister before failing, or the key would be held by a job that
		// never runs.
		delete(r.jobs, key)
		delete(r.byID, job.ID)
		r.mu.Unlock()
		return nil, false, fmt.Errorf("%w (%d waiting)", ErrQueueFull, r.capacity)
	}
	r.mu.Unlock()

	select {
	case r.queue <- job:
		return job, true, nil
	case <-r.baseCtx.Done():
		return nil, false, errors.New("jobs: the runner stopped before the job was queued")
	}
}

// RunOnce runs work under a key, or joins the work already running for it.
//
// It is the shape a caller that does not care about job handles wants: run this
// unless somebody already is. A pass that joined rather than ran returns
// (nil, nil) - not an error, because nothing failed; another caller is simply
// already doing the work. That is what lets a background loop and an API endpoint
// share one answer without either knowing about the other.
func (r *Runner) RunOnce(key string, work func() (any, error)) (any, error) {
	job, isNew, err := r.Submit(key, func(context.Context) (any, error) {
		return work()
	})
	if err != nil {
		return nil, err
	}
	if !isNew {
		// Already queued or running. Joining it and waiting would make this
		// caller's pass as long as the other one's for no benefit, and the
		// periodic loop would then be late for its next tick.
		return nil, nil
	}
	return job.Wait().Value, job.Wait().Err
}

// Job returns a job by id, or nil.
func (r *Runner) Job(id string) *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

// work is the worker loop: take a job, run it, record what happened.
func (r *Runner) work(logger Logger) {
	defer r.wg.Done()

	for job := range r.queue {
		job.mu.Lock()
		job.state = StateRunning
		job.started = time.Now()
		job.mu.Unlock()

		value, err := r.run(job)

		job.mu.Lock()
		job.state = StateDone
		job.ended = time.Now()
		job.result = Result{Value: value, Err: err}
		job.mu.Unlock()
		close(job.done)

		// A finished job stops holding its key, so the same work can be asked
		// for again - a rescan after a scan, a retry after a failure.
		r.mu.Lock()
		if r.jobs[job.Key] == job {
			delete(r.jobs, job.Key)
		}
		r.mu.Unlock()

		logger.Debug("job finished",
			"job_id", job.ID, "key", job.Key, "duration", job.ended.Sub(job.started).String(),
			"error", err)
	}
}

// run executes one job's work and recovers a panic, because a job runs on its
// own goroutine: a panic in one would otherwise take the process with it, and
// the work here parses untrusted files.
func (r *Runner) run(job *Job) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("job %s panicked: %v", job.ID, recovered)
		}
	}()
	return job.work(r.baseCtx)
}

// Close stops accepting work and waits for the running jobs to notice.
//
// It does not wait for them to finish: a job can be a feature-length OCR pass,
// and a shutdown that blocks on one is a shutdown that never happens. The
// context cancellation is what tells them to stop.
func (r *Runner) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	queue := r.queue
	r.mu.Unlock()

	r.cancel()
	close(queue)
	r.wg.Wait()
}

// Logger is the part of slog this package uses, so a test can pass nothing.
type Logger interface {
	Debug(msg string, args ...any)
}

type discardLogger struct{}

func (discardLogger) Debug(string, ...any) {}
