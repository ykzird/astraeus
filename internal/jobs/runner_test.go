package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls until condition holds, so the tests do not depend on sleeps.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRunner_JobOutlivesItsRequest is the regression test for W-2.
//
// A scan used to run under r.Context(), so a UI timeout or a closed tab
// cancelled work that had nothing to do with that client. The job's context
// descends from the runner's instead, and this asserts it by cancelling the
// caller's context and finding the job still finished.
func TestRunner_JobOutlivesItsRequest(t *testing.T) {
	t.Parallel()

	runner := New(context.Background(), Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	// The caller's context stands for a request that goes away. It is cancelled
	// before the job even starts, so if the runner wired the work to anything the
	// caller owns the job would fail immediately.
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	if requestCtx.Err() == nil {
		t.Fatal("the abandoned request context is not cancelled, so this test proves nothing")
	}

	started := make(chan struct{})
	job, isNew, err := runner.Submit("scan:library-1", func(ctx context.Context) (any, error) {
		close(started)
		// The work looks at the only context it has, which is the runner's.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("the job's context was already cancelled: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return "finished", nil
		}
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !isNew {
		t.Fatal("the first submission should have started a job")
	}

	// Note what the call above does not have: a context argument. A caller
	// cannot hand the runner its own context, which is what makes the
	// detachment structural rather than a convention every call site has to
	// remember - the abandoned requestCtx above has nowhere to go.
	<-started

	result := job.Wait()
	if result.Err != nil {
		t.Errorf("the job failed although no caller's context reaches it: %v", result.Err)
	}
	if result.Value != "finished" {
		t.Errorf("job value = %v, want the work's result", result.Value)
	}
}

// TestRunner_StopCancelsJobsButACallerCannot pins the other direction: the only
// thing that cancels a job's context is the runner stopping.
func TestRunner_StopCancelsJobsButACallerCannot(t *testing.T) {
	t.Parallel()

	runner := New(context.Background(), Config{Workers: 1}, nil)

	started := make(chan struct{})

	job, _, err := runner.Submit("scan:library-2", func(ctx context.Context) (any, error) {
		close(started)
		// A real job watches its context so it can stop; one that ignored it
		// would make Close wait for it, which is the behaviour Close documents.
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	// The runner is the thing that owns these contexts. Close waits for the
	// worker, so the work has to be able to notice the cancellation - which is
	// exactly what a real job does, and what this asserts by having the work
	// watch its context rather than a channel of the test's.
	select {
	case <-job.Done():
		t.Fatal("the job finished before the runner was stopped")
	default:
	}

	runner.Close()

	if state := job.State(); state != StateDone {
		t.Errorf("state after Close = %q, want %q", state, StateDone)
	}
}

// TestRunner_DeduplicatesByKey is the regression test for the duplicate-work
// half of L-13 and A-B7: N players on one subtitle track started N OCR passes,
// and two scans of one library raced.
func TestRunner_DeduplicatesByKey(t *testing.T) {
	t.Parallel()

	runner := New(context.Background(), Config{Workers: 2}, nil)
	t.Cleanup(runner.Close)

	var runs int32
	release := make(chan struct{})

	work := func(context.Context) (any, error) {
		atomic.AddInt32(&runs, 1)
		<-release
		return "done", nil
	}

	first, isNew, err := runner.Submit("ocr:entity-1:track-2", work)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !isNew {
		t.Fatal("the first submission should be new work")
	}

	// Ten more callers ask for the same key while it is running.
	for i := 0; i < 10; i++ {
		again, isNew, err := runner.Submit("ocr:entity-1:track-2", work)
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
		if isNew {
			t.Errorf("submission %d started a second job for a key that is already running", i)
		}
		if again.ID != first.ID {
			t.Errorf("submission %d joined job %s, want %s", i, again.ID, first.ID)
		}
	}

	close(release)
	first.Wait()

	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Errorf("the work ran %d times for one key, want 1", got)
	}

	// A different key is different work, and the same key may be asked for again
	// once it has finished - which is what a rescan after a scan is.
	if _, isNew, err := runner.Submit("ocr:entity-1:track-3", work); err != nil || !isNew {
		t.Errorf("a different key should be new work: new=%v err=%v", isNew, err)
	}
	waitFor(t, "the second key to run", func() bool { return atomic.LoadInt32(&runs) == 2 })
	release2 := make(chan struct{})
	close(release2)

	job, isNew, err := runner.Submit("scan:library-9", func(context.Context) (any, error) { return nil, nil })
	if err != nil || !isNew {
		t.Fatalf("Submit: new=%v err=%v", isNew, err)
	}
	job.Wait()
	again, isNew, err := runner.Submit("scan:library-9", func(context.Context) (any, error) { return nil, nil })
	if err != nil {
		t.Fatalf("Submit after completion: %v", err)
	}
	if !isNew {
		t.Error("a finished job should not hold its key forever")
	}
	again.Wait()
}

// TestRunner_BoundsConcurrency is the other half: a loop of requests must not be
// able to start unbounded work.
func TestRunner_BoundsConcurrency(t *testing.T) {
	t.Parallel()

	const workers = 2
	runner := New(context.Background(), Config{Workers: workers, Queue: 32}, nil)
	t.Cleanup(runner.Close)

	var (
		mu      sync.Mutex
		running int
		peak    int
	)
	release := make(chan struct{})

	for i := 0; i < 12; i++ {
		key := fmt.Sprintf("scan:library-%d", i)
		if _, _, err := runner.Submit(key, func(context.Context) (any, error) {
			mu.Lock()
			running++
			if running > peak {
				peak = running
			}
			mu.Unlock()

			<-release

			mu.Lock()
			running--
			mu.Unlock()
			return nil, nil
		}); err != nil {
			t.Fatalf("Submit %s: %v", key, err)
		}
	}

	waitFor(t, "the workers to be busy", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return peak == workers
	})
	close(release)

	waitFor(t, "every job to finish", func() bool {
		for i := 0; i < 12; i++ {
			if job := runner.Job(fmt.Sprintf("job-%d", i+1)); job != nil && job.State() != StateDone {
				return false
			}
		}
		return true
	})

	mu.Lock()
	defer mu.Unlock()
	if peak > workers {
		t.Errorf("%d jobs ran at once, want at most %d", peak, workers)
	}
}

// TestRunner_RefusesWhenTheQueueIsFull guards the resource bound: a caller
// looping on an endpoint gets an answer rather than growing a queue forever.
//
// Filling the queue exactly is a race - the worker is taking jobs off it while
// the test puts them on - so this submission loop keeps going until one is
// refused, which is the behaviour under test. The bound is what matters, not
// which submission hits it.
func TestRunner_RefusesWhenTheQueueIsFull(t *testing.T) {
	t.Parallel()

	const queue = 2
	runner := New(context.Background(), Config{Workers: 1, Queue: queue}, nil)
	// Closing the runner unblocks the work below, so a failure here cannot hang
	// the test binary.
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		runner.Close()
	})

	block := func(ctx context.Context) (any, error) {
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	var refused error
	accepted := 0
	for i := 0; i < queue+8; i++ {
		_, _, err := runner.Submit(fmt.Sprintf("k%d", i), block)
		if err != nil {
			refused = err
			break
		}
		accepted++
	}

	if !errors.Is(refused, ErrQueueFull) {
		t.Fatalf("submitted %d jobs without ever being refused, want ErrQueueFull "+
			"once the %d-deep queue is full (err=%v)", accepted, queue, refused)
	}
	if accepted == 0 {
		t.Fatal("the first submission was refused, so the bound is one job too tight")
	}

	// The refused key must not be left registered, or the same work could never
	// be asked for again.
	waitFor(t, "the refused key to be absent", func() bool {
		return runner.Job(fmt.Sprintf("job-%d", accepted+1)) == nil
	})
}

// TestRunner_RecordsFailureAndRecoversAPanic covers the two ways a job ends
// badly, because a runner that loses either would report success wrongly.
func TestRunner_RecordsFailureAndRecoversAPanic(t *testing.T) {
	t.Parallel()

	runner := New(context.Background(), Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	wantErr := errors.New("the provider refused")
	failed, _, err := runner.Submit("enrich:fail", func(context.Context) (any, error) {
		return nil, wantErr
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result := failed.Wait(); !errors.Is(result.Err, wantErr) {
		t.Errorf("failed job result = %+v, want the work's error", result)
	}

	// A panic must be reported as that job failing, not take the process down:
	// the work here parses untrusted files.
	panicking, _, err := runner.Submit("enrich:panic", func(context.Context) (any, error) {
		panic("the parser found something it did not expect")
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	result := panicking.Wait()
	if result.Err == nil {
		t.Fatal("a panicking job reported success")
	}
	if !containsText(result.Err.Error(), "panicked") {
		t.Errorf("panic error = %v, want it to say the job panicked", result.Err)
	}
}

// TestRunner_CloseCancelsRunningWork pins what stopping does: it tells the jobs
// to stop and does not wait for a long one.
func TestRunner_CloseCancelsRunningWork(t *testing.T) {
	t.Parallel()

	runner := New(context.Background(), Config{Workers: 1}, nil)

	started := make(chan struct{})
	cancelled := make(chan struct{})
	job, _, err := runner.Submit("ocr:long", func(ctx context.Context) (any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	<-started
	runner.Close()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the runner did not cancel the job's context")
	}
	if result := job.Wait(); result.Err == nil {
		t.Error("a cancelled job should report the cancellation")
	}
}

func containsText(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestRunner_RunOnceJoinsInsteadOfRepeating is the regression test for the
// overlap half of A-B7.
//
// The background enrichment loop and the API both drive the same worker, so a
// tick that lands while a manual pass is running must do nothing rather than
// walk the same incomplete entities and call the metadata provider once more for
// each. RunOnce is the shared answer: it runs the work or joins it, and a caller
// that joined is told nothing happened rather than being made to wait.
func TestRunner_RunOnceJoinsInsteadOfRepeating(t *testing.T) {
	t.Parallel()

	runner := New(context.Background(), Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	var runs int32
	release := make(chan struct{})
	work := func() (any, error) {
		atomic.AddInt32(&runs, 1)
		<-release
		return "pass complete", nil
	}

	// The first caller runs it. RunOnce waits, so it has to be in a goroutine.
	done := make(chan struct{})
	var firstValue any
	var firstErr error
	go func() {
		defer close(done)
		firstValue, firstErr = runner.RunOnce("enrich:all", work)
	}()

	waitFor(t, "the first pass to start", func() bool {
		return atomic.LoadInt32(&runs) == 1
	})

	// The loop ticks while it is in flight. It must join rather than queue a
	// second pass and wait for its turn on the same worker - which is what makes
	// a completed background pass able to queue a re-run before its predecessor
	// has released the worker. `value` is nil because a joining caller did not
	// run the work, so there is no result of its own to report.
	value, err := runner.RunOnce("enrich:all", work)
	if err != nil {
		t.Errorf("joining an in-flight pass = %v, want no error: nothing failed", err)
	}
	if value != nil {
		t.Errorf("joining an in-flight pass returned a result, want none: it did not run")
	}

	close(release)
	<-done
	if firstErr != nil {
		t.Fatalf("the first pass failed: %v", firstErr)
	}
	if firstValue != "pass complete" {
		t.Errorf("the pass that ran returned %v", firstValue)
	}
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Errorf("the work ran %d times, want 1: a tick during a manual pass must do nothing", got)
	}
}

// TestRunner_RunOnceRunsAgainAfterCompletion guards the other direction: the
// guard must not be a one-shot latch, or the periodic loop would enrich once and
// never again.
func TestRunner_RunOnceRunsAgainAfterCompletion(t *testing.T) {
	t.Parallel()

	runner := New(context.Background(), Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	var runs int32
	work := func() (any, error) {
		atomic.AddInt32(&runs, 1)
		return "ok", nil
	}

	for i := 0; i < 3; i++ {
		value, err := runner.RunOnce("enrich:all", work)
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if value != "ok" {
			t.Fatalf("pass %d returned %v, want the work's value", i, value)
		}
	}
	if got := atomic.LoadInt32(&runs); got != 3 {
		t.Errorf("sequential passes ran %d times, want 3", got)
	}
}
