package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/ykzird/astraeus/internal/access"
	"github.com/ykzird/astraeus/internal/observability"
)

// progressList is the shape GET /api/progress answers with.
type progressList struct {
	Entries []progressEntryResource `json:"entries"`
}

// newProxyGate builds the gate the binary would run behind a trusted access
// proxy. httptest requests carry 192.0.2.1 as their peer address, so trusting
// that test-net range is what lets a forwarded identity header be believed.
func newProxyGate(t *testing.T) *access.Gate {
	t.Helper()

	gate, err := access.New(access.Config{
		Mode:           access.ModeProxy,
		IdentityHeader: access.HeaderTailscaleLogin,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		Metrics:        observability.New(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("building the test access gate: %v", err)
	}
	return gate
}

func requireStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()
	if recorder.Code != want {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, want, recorder.Body.String())
	}
}

// TestPlaybackProgress_IsPerViewer is the regression test for the largest
// simplification the product still had: progress was keyed by entity alone, so
// two viewers of the same film shared one position and the second report
// overwrote the first viewer's place.
//
// The identity is the one the access gate attaches to the request, not anything
// the client sends directly, which is why this exercises the real middleware.
func TestPlaybackProgress_IsPerViewer(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	env.useGate(newProxyGate(t))
	entity, _ := seedPlayableEntity(t, env, "Arrival (2016).mp4", "mp4 bytes")

	const alice, bob = "alice@example.com", "bob@example.com"

	// Alice is ten minutes in. Bob then starts the same film and stops twenty
	// minutes in, which is the report that used to erase Alice's place.
	requireStatus(t, env.doAs(t, alice, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 600, "duration_seconds": 7000}`), http.StatusNoContent)
	requireStatus(t, env.doAs(t, bob, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 1200, "duration_seconds": 7000}`), http.StatusNoContent)

	aliceDetail := decodeBody[entityDetail](t, env.doAs(t, alice, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if aliceDetail.Progress == nil || aliceDetail.Progress.PositionSeconds != 600 {
		t.Errorf("alice resumes at %+v, want her own 600: bob's report must not move her", aliceDetail.Progress)
	}
	bobDetail := decodeBody[entityDetail](t, env.doAs(t, bob, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if bobDetail.Progress == nil || bobDetail.Progress.PositionSeconds != 1200 {
		t.Errorf("bob resumes at %+v, want his own 1200", bobDetail.Progress)
	}

	// The continue-watching list is a question each viewer asks about
	// themselves, so it must never show the other viewer's row.
	aliceList := decodeBody[progressList](t, env.doAs(t, alice, http.MethodGet, "/api/progress", ""))
	if len(aliceList.Entries) != 1 || aliceList.Entries[0].Progress.PositionSeconds != 600 {
		t.Errorf("alice's continue-watching list = %+v, want only her 600", aliceList.Entries)
	}
	bobList := decodeBody[progressList](t, env.doAs(t, bob, http.MethodGet, "/api/progress", ""))
	if len(bobList.Entries) != 1 || bobList.Entries[0].Progress.PositionSeconds != 1200 {
		t.Errorf("bob's continue-watching list = %+v, want only his 1200", bobList.Entries)
	}

	// Starting over forgets the caller's place and nobody else's.
	requireStatus(t, env.doAs(t, alice, http.MethodDelete, "/api/entities/"+entity.ID+"/progress", ""),
		http.StatusNoContent)
	bobDetail = decodeBody[entityDetail](t, env.doAs(t, bob, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if bobDetail.Progress == nil || bobDetail.Progress.PositionSeconds != 1200 {
		t.Errorf("clearing alice's place moved bob's: %+v", bobDetail.Progress)
	}
	aliceDetail = decodeBody[entityDetail](t, env.doAs(t, alice, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if aliceDetail.Progress != nil {
		t.Errorf("alice's cleared place survived: %+v", aliceDetail.Progress)
	}
}

// TestPlaybackProgress_FinishingIsOnlyTheFinishersBusiness covers the finished
// rule across viewers: one viewer watching a film through must not clear the
// place another viewer is still in the middle of.
func TestPlaybackProgress_FinishingIsOnlyTheFinishersBusiness(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	env.useGate(newProxyGate(t))
	entity, _ := seedPlayableEntity(t, env, "Arrival (2016).mp4", "mp4 bytes")

	const alice, bob = "alice@example.com", "bob@example.com"

	requireStatus(t, env.doAs(t, alice, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 3000, "duration_seconds": 7000}`), http.StatusNoContent)
	// Bob reports a position in the closing fraction, which clears his own row.
	requireStatus(t, env.doAs(t, bob, http.MethodPut, "/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 6900, "duration_seconds": 7000}`), http.StatusNoContent)

	aliceDetail := decodeBody[entityDetail](t, env.doAs(t, alice, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if aliceDetail.Progress == nil || aliceDetail.Progress.PositionSeconds != 3000 {
		t.Errorf("alice resumes at %+v, want her 3000 untouched by bob finishing", aliceDetail.Progress)
	}
	bobDetail := decodeBody[entityDetail](t, env.doAs(t, bob, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if bobDetail.Progress != nil {
		t.Errorf("bob finished the film but kept progress %+v", bobDetail.Progress)
	}
}

// TestPlaybackProgress_AnUngatedServerIsOneViewer pins the other end: with no
// gate there are no viewers to tell apart, and a header a client made up must
// not be believed as one. The unnamed viewer is nameable, but it is one.
func TestPlaybackProgress_AnUngatedServerIsOneViewer(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	entity, _ := seedPlayableEntity(t, env, "Arrival (2016).mp4", "mp4 bytes")

	// No gate is installed, so this header reaches no one who trusts it.
	requireStatus(t, env.doAs(t, "somebody@example.com", http.MethodPut,
		"/api/entities/"+entity.ID+"/progress",
		`{"position_seconds": 600, "duration_seconds": 7000}`), http.StatusNoContent)

	detail := decodeBody[entityDetail](t, env.do(t, http.MethodGet, "/api/entities/"+entity.ID, ""))
	if detail.Progress == nil || detail.Progress.PositionSeconds != 600 {
		t.Errorf("the ungated server's single viewer sees %+v, want the reported 600", detail.Progress)
	}
}
