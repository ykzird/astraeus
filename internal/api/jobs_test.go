package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/jobs"
)

// withJobs installs a runner on the test server.
func withJobs(runner *jobs.Runner) envOption {
	return func(deps *Deps) { deps.Jobs = runner }
}

// waitFor polls until condition holds.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestScanAll_AcceptsTheWorkAndReportsIt is the API half of W-2.
//
// A scan used to hold the request open until it finished, so the UI's fifteen
// second timeout ended the request and the server cancelled the scan with it.
// The handler now answers 202 with a job to poll, and the work runs whether or
// not the caller is still there.
func TestScanAll_AcceptsTheWorkAndReportsIt(t *testing.T) {
	t.Parallel()

	runner := jobs.New(context.Background(), jobs.Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	env := newTestEnv(t, withJobs(runner))
	lib := createLibrary(t, env, "Movies", t.TempDir(), "movies")

	recorder := env.do(t, http.MethodPost, "/api/scan", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /api/scan = %d, want 202 Accepted (body %s)",
			recorder.Code, recorder.Body.String())
	}

	var accepted jobResource
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decoding the accepted body: %v", err)
	}
	if accepted.JobID == "" || accepted.URL == "" {
		t.Fatalf("the 202 body names no job: %+v", accepted)
	}
	if accepted.Key != "scan:all" {
		t.Errorf("key = %q, want scan:all", accepted.Key)
	}
	if location := recorder.Header().Get("Location"); location != accepted.URL {
		t.Errorf("Location = %q, want %q", location, accepted.URL)
	}

	// The job is pollable, and it finishes.
	var status jobStatus
	waitFor(t, "the job to finish", func() bool {
		recorder := env.do(t, http.MethodGet, accepted.URL, "")
		if recorder.Code != http.StatusOK {
			return false
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
			return false
		}
		return status.State == jobs.StateDone
	})
	if status.Error != "" {
		t.Errorf("the scan failed: %s", status.Error)
	}
	if status.Result == nil {
		t.Error("a finished job reported no result")
	}
	_ = lib
}

// TestScanAll_JoinsAJobAlreadyRunning covers the deduplication as a client sees
// it: two presses of Scan are one scan, and the second caller is told so.
func TestScanAll_JoinsAJobAlreadyRunning(t *testing.T) {
	t.Parallel()

	runner := jobs.New(context.Background(), jobs.Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	// Hold the single worker so the first scan cannot finish while the second
	// request arrives.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blocking, _, err := runner.Submit("hold", func(ctx context.Context) (any, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("holding the worker: %v", err)
	}
	_ = blocking

	env := newTestEnv(t, withJobs(runner))
	createLibrary(t, env, "Movies", t.TempDir(), "movies")

	first := env.do(t, http.MethodPost, "/api/scan", "")
	if first.Code != http.StatusAccepted {
		t.Fatalf("the first scan = %d, want 202", first.Code)
	}
	var firstJob jobResource
	if err := json.Unmarshal(first.Body.Bytes(), &firstJob); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !firstJob.New {
		t.Error("the first submission should report itself as new work")
	}

	second := env.do(t, http.MethodPost, "/api/scan", "")
	if second.Code != http.StatusAccepted {
		t.Fatalf("the second scan = %d, want 202", second.Code)
	}
	var secondJob jobResource
	if err := json.Unmarshal(second.Body.Bytes(), &secondJob); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if secondJob.JobID != firstJob.JobID {
		t.Errorf("the second scan started job %s, want it to join %s",
			secondJob.JobID, firstJob.JobID)
	}
	if secondJob.New {
		t.Error("the second submission reported itself as new work")
	}
}

// TestGetJob_UnknownID covers the poll that arrives too late, which is the
// ordinary end of a job's life rather than an error worth alarming about.
func TestGetJob_UnknownID(t *testing.T) {
	t.Parallel()

	runner := jobs.New(context.Background(), jobs.Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	env := newTestEnv(t, withJobs(runner))
	recorder := env.do(t, http.MethodGet, "/api/jobs/job-999", "")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("polling an unknown job = %d, want 404", recorder.Code)
	}
}

// TestJobs_WithoutARunnerStillWorks guards the fallback: a server with no runner
// runs the work inline, which is what every one of these endpoints did before
// the runner existed, so an embedder or a test is not forced to have one.
func TestJobs_WithoutARunnerStillWorks(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	createLibrary(t, env, "Movies", t.TempDir(), "movies")

	recorder := env.do(t, http.MethodPost, "/api/scan", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /api/scan without a runner = %d, want 200 with the result inline",
			recorder.Code)
	}

	// And the status route says so rather than pretending.
	recorder = env.do(t, http.MethodGet, "/api/jobs/job-1", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("polling with no runner = %d, want 503", recorder.Code)
	}
}

// TestScanLibrary_DedupesByLibrary checks the key is per library, so two
// libraries scan in parallel while one library does not scan twice.
func TestScanLibrary_DedupesByLibrary(t *testing.T) {
	t.Parallel()

	runner := jobs.New(context.Background(), jobs.Config{Workers: 1}, nil)
	t.Cleanup(runner.Close)

	env := newTestEnv(t, withJobs(runner))
	first := createLibrary(t, env, "First", t.TempDir(), "movies")
	second := createLibrary(t, env, "Second", t.TempDir(), "movies")

	// Hold the worker so neither can finish.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	_, _, err := runner.Submit("hold", func(ctx context.Context) (any, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("holding the worker: %v", err)
	}

	body := func(id string) jobResource {
		t.Helper()
		recorder := env.do(t, http.MethodPost, "/api/libraries/"+id+"/scan", "")
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("scanning %s = %d, want 202", id, recorder.Code)
		}
		var resource jobResource
		if err := json.Unmarshal(recorder.Body.Bytes(), &resource); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return resource
	}

	firstJob := body(first.ID)
	if firstJob.Key != "scan:"+first.ID {
		t.Errorf("key = %q, want scan:%s", firstJob.Key, first.ID)
	}

	// The same library again joins; a different one is its own job.
	if again := body(first.ID); again.JobID != firstJob.JobID {
		t.Errorf("scanning one library twice started two jobs: %s and %s",
			firstJob.JobID, again.JobID)
	}
	if other := body(second.ID); other.JobID == firstJob.JobID {
		t.Error("two libraries shared one scan job")
	}
}

// TestJobs_QueueFullIsARetryableRefusal covers the resource bound as a client
// sees it: a full queue is 503 with Retry-After, not a 500.
//
// The queue is filled directly rather than through the scan endpoints, because
// those deduplicate by library - scanning one library twice is one job, which is
// the point of the keys and means no number of requests for one library can fill
// a queue. What is under test here is the refusal's shape, so the queue is the
// only thing that has to be real.
func TestJobs_QueueFullIsARetryableRefusal(t *testing.T) {
	t.Parallel()

	runner := jobs.New(context.Background(), jobs.Config{Workers: 1, Queue: 1}, nil)
	t.Cleanup(runner.Close)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	block := func(ctx context.Context) (any, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}

	// One job holds the only worker and one waits, which is exactly the queue's
	// depth - so the next distinct key has nowhere to go. Filling it by looping
	// until a refusal does not work: the worker takes a job off the queue as fast
	// as the loop puts them on, so the queue is rarely at its depth when the API
	// call arrives.
	holding, _, err := runner.Submit("holding-the-worker", block)
	if err != nil {
		t.Fatalf("holding the worker: %v", err)
	}
	waitFor(t, "the worker to be busy", func() bool { return holding.State() == jobs.StateRunning })
	if _, _, err := runner.Submit("waiting", block); err != nil {
		t.Fatalf("filling the queue: %v", err)
	}

	// Now the API's answer for that condition.
	env := newTestEnv(t, withJobs(runner))
	createLibrary(t, env, "Movies", t.TempDir(), "movies")

	recorder := env.do(t, http.MethodPost, "/api/scan", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("a full queue answered %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("a busy refusal should say when to retry")
	}
}
