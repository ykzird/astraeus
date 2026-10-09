package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ykzird/astraeus/internal/access"
	"github.com/ykzird/astraeus/internal/library"
	"github.com/ykzird/astraeus/internal/streaming"
)

// testPolicy parses a policy for a test, which is how a test states who may see
// what without a file on disk.
func testPolicy(t *testing.T, text string) *access.Policy {
	t.Helper()

	policy, err := access.ParsePolicy(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parsing test policy: %v\n---\n%s", err, text)
	}
	return policy
}

func withPolicy(policy *access.Policy) envOption {
	return func(deps *Deps) { deps.Policy = policy }
}

// seedLibraryWithEntity registers a library holding one file and returns both,
// so a test can grant one library and hide another. It acts as the default test
// identity, which is why a policy under test lists that identity as an admin
// *and* grants it "*": being an admin is permission to change the library, not
// to see it, so an operator who wants both says both.
func seedLibraryWithEntity(t *testing.T, env *testEnv, name, fileName string) (library.Library, library.MediaEntity) {
	t.Helper()

	root := t.TempDir()
	writeMediaFile(t, filepath.Join(root, fileName), "media bytes")

	lib := createLibrary(t, env, name, root, "movies")
	env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")

	recorder := env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing entities in %q: status %d body %s", name, recorder.Code, recorder.Body.String())
	}
	entities := decodeBody[[]library.MediaEntity](t, recorder)
	if len(entities) == 0 {
		t.Fatalf("scanning %q produced no entities", name)
	}
	return lib, entities[0]
}

// objectsOf reads an entity's objects straight from the repository: a test needs
// a hidden entity's object id, and the API is exactly what will not hand it to
// the viewer under test.
func objectsOf(t *testing.T, env *testEnv, entityID string) []library.MediaObject {
	t.Helper()

	objects, err := env.repo.GetObjectsByEntity(context.Background(), entityID)
	if err != nil {
		t.Fatalf("reading objects of %s: %v", entityID, err)
	}
	if len(objects) == 0 {
		t.Fatalf("entity %s has no objects", entityID)
	}
	return objects
}

// TestAccessPolicy_LibraryGrantsHoldOnEveryReadPath is the feature itself: a
// viewer granted one library must not reach another through any route, and a
// library it may not see must be indistinguishable from one that does not
// exist.
func TestAccessPolicy_LibraryGrantsHoldOnEveryReadPath(t *testing.T) {
	t.Parallel()

	const alice = "alice@example.com"

	// The stub prober is what makes the playback route reachable at all; the
	// refusal under test happens before any media is touched.
	prober := stubProber{info: &streaming.MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080,
	}}
	env := newTestEnv(t, withProber(prober), withPolicy(testPolicy(t, `
admin: tester@example.com
tester@example.com: *
alice@example.com: Movies
`)))
	env.useGate(newProxyGate(t))

	_, moviesEntity := seedLibraryWithEntity(t, env, "Movies", "Arrival (2016).mkv")
	kidsLibrary, kidsEntity := seedLibraryWithEntity(t, env, "Kids", "Bluey S01E01.mkv")

	moviesObjects := objectsOf(t, env, moviesEntity.ID)
	kidsObjects := objectsOf(t, env, kidsEntity.ID)

	libraries := decodeBody[[]library.Library](t, env.doAs(t, alice, http.MethodGet, "/api/libraries", ""))
	if len(libraries) != 1 || libraries[0].Name != "Movies" {
		t.Errorf("libraries = %+v, want only Movies", libraries)
	}

	entities := decodeBody[[]library.MediaEntity](t, env.doAs(t, alice, http.MethodGet, "/api/entities", ""))
	if len(entities) != 1 || entities[0].ID != moviesEntity.ID {
		t.Errorf("entities = %+v, want only the Movies entity", entities)
	}

	// What the viewer may see behaves normally.
	requireStatus(t, env.doAs(t, alice, http.MethodGet, "/api/entities/"+moviesEntity.ID, ""), http.StatusOK)
	requireStatus(t, env.doAs(t, alice, http.MethodGet, "/api/objects/"+moviesObjects[0].ID+"/file", ""), http.StatusOK)

	// Everything pointing into the other library is a 404, not a 403: answering
	// "forbidden" for a real id and "not found" for an invented one is how an
	// API tells a stranger which ids exist.
	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "the library", method: http.MethodGet, path: "/api/libraries/" + kidsLibrary.ID},
		{name: "its entities", method: http.MethodGet, path: "/api/libraries/" + kidsLibrary.ID + "/entities"},
		{name: "an entity in it", method: http.MethodGet, path: "/api/entities/" + kidsEntity.ID},
		{name: "negotiating it", method: http.MethodPost, path: "/api/entities/" + kidsEntity.ID + "/playback"},
		{name: "reporting progress on it", method: http.MethodPut, path: "/api/entities/" + kidsEntity.ID + "/progress"},
		{name: "its media file", method: http.MethodGet, path: "/api/objects/" + kidsObjects[0].ID + "/file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requireStatus(t, env.doAs(t, alice, tt.method, tt.path, ""), http.StatusNotFound)
		})
	}
}

func TestAccessPolicy_MutationsRequireAnAdmin(t *testing.T) {
	t.Parallel()

	const alice = "alice@example.com"

	env := newTestEnv(t, withPolicy(testPolicy(t, `
admin: tester@example.com
tester@example.com: *
alice@example.com: Movies
`)))
	env.useGate(newProxyGate(t))

	lib, _ := seedLibraryWithEntity(t, env, "Movies", "Arrival (2016).mp4")

	refused := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "registering a library", method: http.MethodPost, path: "/api/libraries",
			body: `{"name":"Sneaky","path":"` + t.TempDir() + `","kind":"movies"}`},
		{name: "scanning one library", method: http.MethodPost, path: "/api/libraries/" + lib.ID + "/scan"},
		{name: "scanning everything", method: http.MethodPost, path: "/api/scan"},
		{name: "enriching metadata", method: http.MethodPost, path: "/api/metadata/enrich"},
		{name: "removing a library", method: http.MethodDelete, path: "/api/libraries/" + lib.ID},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recorder := env.doAs(t, alice, tt.method, tt.path, tt.body)
			requireStatus(t, recorder, http.StatusForbidden)
			if code := decodeBody[map[string]string](t, recorder)["code"]; code != "admin_required" {
				t.Errorf("code = %q, want admin_required", code)
			}
		})
	}

	// The refusal has to be a refusal: the library alice tried to remove is
	// still there.
	if _, err := env.repo.GetLibrary(context.Background(), lib.ID); err != nil {
		t.Errorf("reading the library alice tried to remove: %v", err)
	}

	// And an admin is unaffected by any of it.
	requireStatus(t, env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", ""), http.StatusOK)
	requireStatus(t, env.do(t, http.MethodPost, "/api/scan", ""), http.StatusOK)
	requireStatus(t, env.do(t, http.MethodPost, "/api/metadata/enrich", ""), http.StatusOK)
}

// A position can outlive the grant that allowed it. Revoking access to a library
// must not leave its titles sitting in Continue watching, and filtering has to
// be selective rather than emptying the list.
func TestAccessPolicy_RevokedLibraryLeavesContinueWatching(t *testing.T) {
	t.Parallel()

	const alice = "alice@example.com"

	env := newTestEnv(t, withPolicy(testPolicy(t, `
admin: tester@example.com
tester@example.com: *
alice@example.com: Movies
`)))
	env.useGate(newProxyGate(t))

	_, moviesEntity := seedLibraryWithEntity(t, env, "Movies", "Arrival (2016).mp4")
	_, kidsEntity := seedLibraryWithEntity(t, env, "Kids", "Bluey S01E01.mp4")

	ctx := context.Background()
	for _, entity := range []library.MediaEntity{moviesEntity, kidsEntity} {
		if err := env.repo.SaveProgress(ctx, &library.PlaybackProgress{
			ViewerID:        alice,
			EntityID:        entity.ID,
			PositionSeconds: 600,
			DurationSeconds: 7000,
		}); err != nil {
			t.Fatalf("seeding a position for %s: %v", entity.ID, err)
		}
	}

	entries := decodeBody[progressList](t, env.doAs(t, alice, http.MethodGet, "/api/progress", ""))
	if len(entries.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: only the visible library's position belongs in the list", len(entries.Entries))
	}
	if entries.Entries[0].Entity.ID != moviesEntity.ID {
		t.Errorf("entry is %s, want the Movies entity", entries.Entries[0].Entity.ID)
	}
}

// A session URL is a capability. It is handed to a viewer that was allowed to
// negotiate, but it can be passed on or a grant can be revoked afterwards, so
// the playlist route asks again rather than trusting the negotiation.
func TestAccessPolicy_PlaylistRouteRechecksTheLibrary(t *testing.T) {
	t.Parallel()

	const alice, bob = "alice@example.com", "bob@example.com"

	prober := stubProber{info: &streaming.MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080,
	}}
	streams := &fakeStreams{}
	env := newTestEnv(t, withProber(prober), withStreams(streams), withPolicy(testPolicy(t, `
admin: tester@example.com
tester@example.com: *
alice@example.com: Movies
bob@example.com: Kids
`)))
	env.useGate(newProxyGate(t))

	_, kidsEntity := seedLibraryWithEntity(t, env, "Kids", "Bluey S01E01.mkv")

	response := decodeBody[playbackResponse](t,
		env.doAs(t, bob, http.MethodPost, "/api/entities/"+kidsEntity.ID+"/playback", ""))
	if response.SessionID == "" {
		t.Fatalf("bob's negotiation started no session: %+v", response)
	}

	playlist := "/hls/" + response.SessionID + "/playlist.m3u8"
	requireStatus(t, env.doAs(t, bob, http.MethodGet, playlist, ""), http.StatusOK)
	requireStatus(t, env.doAs(t, alice, http.MethodGet, playlist, ""), http.StatusNotFound)
	requireStatus(t, env.doAs(t, alice, http.MethodGet, "/hls/no-such-session/playlist.m3u8", ""), http.StatusNotFound)
}

// The regression guard for everything above: an install that has not written a
// policy behaves exactly as it did before the feature existed.
func TestAccessPolicy_UnconfiguredInstallIsUnchanged(t *testing.T) {
	t.Parallel()

	const stranger = "stranger@example.com"

	env := newTestEnv(t)
	env.useGate(newProxyGate(t))

	seedLibraryWithEntity(t, env, "Movies", "Arrival (2016).mp4")
	seedLibraryWithEntity(t, env, "Kids", "Bluey S01E01.mp4")

	libraries := decodeBody[[]library.Library](t, env.doAs(t, stranger, http.MethodGet, "/api/libraries", ""))
	if len(libraries) != 2 {
		t.Errorf("libraries = %d, want both: with no policy every admitted viewer sees everything", len(libraries))
	}

	requireStatus(t, env.doAs(t, stranger, http.MethodPost, "/api/libraries",
		`{"name":"Another","path":"`+t.TempDir()+`","kind":"movies"}`), http.StatusCreated)
}
