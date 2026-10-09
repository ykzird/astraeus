package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ykzird/astraeus/internal/jobs"
)

// timeFormat is the timestamp format the API uses everywhere else.
const timeFormat = time.RFC3339

// jobResource is what a client is told when work has been accepted.
//
// It is deliberately small: an id to poll, where to poll it, and whether the
// work was already running. That last field is what lets a caller tell "I
// started this" from "somebody else already had", which matters when two
// viewers press Scan at the same moment.
type jobResource struct {
	JobID string     `json:"job_id"`
	Key   string     `json:"key"`
	State jobs.State `json:"state"`
	New   bool       `json:"new"`
	URL   string     `json:"status_url"`
}

// startJob submits work to the runner and answers 202 Accepted, or reports
// false so the caller runs the work itself.
//
// The 202 is the point of the whole exercise (W-2 of the 2026-10-09 review): the
// handler used to hold the request open for the whole scan, so the UI's
// fifteen-second timeout ended the request, the server cancelled the work with
// it, and the viewer was told the scan had failed. Answering immediately and
// running the work on the runner's own context decouples the two - the scan
// finishes whether or not anyone is still listening, and the UI polls.
//
// A server with no runner configured falls through to running the work inline,
// which keeps the tests and any embedding that does not want a runner working.
func (s *Server) startJob(w http.ResponseWriter, r *http.Request, key string, work func(context.Context) (any, error)) bool {
	if s.jobs == nil {
		return false
	}

	job, isNew, err := s.jobs.Submit(key, work)
	if err != nil {
		// A full queue is a refusal, not a failure: the caller may retry, and
		// 503 with Retry-After says so more honestly than a 500.
		if errors.Is(err, jobs.ErrQueueFull) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "jobs_busy",
				"too much other work is already queued; try again shortly")
			return true
		}
		writeError(w, http.StatusInternalServerError, "job_rejected", err.Error())
		return true
	}

	location := "/api/jobs/" + job.ID
	w.Header().Set("Location", location)
	writeJSON(w, http.StatusAccepted, jobResource{
		JobID: job.ID,
		Key:   job.Key,
		State: job.State(),
		New:   isNew,
		URL:   location,
	})
	return true
}

// jobStatus is what a client polling a job is told.
type jobStatus struct {
	JobID string     `json:"job_id"`
	Key   string     `json:"key"`
	State jobs.State `json:"state"`
	// StartedAt and EndedAt are RFC 3339, and EndedAt is absent while running.
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	// Result is the work's own value once it has finished. It is omitted while
	// the job runs, and Error replaces it on failure.
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// handleGetJob reports a job's state and, once it has finished, its result.
//
// A job is remembered only while it is queued or running plus the moment it
// ends, so a poll that arrives after that gets 404 and the caller learns to look
// at the library instead. That is deliberate: the result is a scan summary, and
// the thing a client actually wants - the new state of the library - is in the
// library.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_unavailable",
			"this server does not run jobs")
		return
	}

	job := s.jobs.Job(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, "job_not_found",
			"no job with that id is known; it may have finished, or the server may have restarted")
		return
	}

	status := jobStatus{
		JobID: job.ID,
		Key:   job.Key,
		State: job.State(),
	}
	if started := job.StartedAt(); !started.IsZero() {
		status.StartedAt = started.Format(timeFormat)
	}

	if job.State() == jobs.StateDone {
		result := job.Wait()
		if ended := job.EndedAt(); !ended.IsZero() {
			status.EndedAt = ended.Format(timeFormat)
		}
		if result.Err != nil {
			status.Error = result.Err.Error()
		} else {
			status.Result = result.Value
		}
	}
	writeJSON(w, http.StatusOK, status)
}
