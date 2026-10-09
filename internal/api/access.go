package api

import (
	"context"
	"net/http"

	"github.com/ykzird/astraeus/internal/access"
	"github.com/ykzird/astraeus/internal/library"
)

// viewerAccess is one request's view of the library: the viewer the access gate
// authenticated, resolved against the operator's policy.
//
// Every viewer-facing read goes through here rather than through the repository
// directly, because the check and the read have to be the same operation. A
// handler that loaded an entity and then asked "may I?" could answer with
// something it had already read, and a handler that forgot to ask is a leak
// nobody notices. Going through one module makes the lookup and the refusal one
// call, and makes a library the viewer may not see indistinguishable from one
// that does not exist.
//
// The mutating operations are not scoped here: they are the admin operations,
// and they are gated by isAdmin rather than filtered.
type viewerAccess struct {
	viewer string
	policy *access.Policy
	repo   library.Repository

	// libraryNames maps a library id to its name, because a policy grant may
	// name a library either way. It is read at most once per request, and not
	// at all when there is no policy to match against.
	libraryNames map[string]string
}

// access resolves what this request may see.
func (s *Server) access(r *http.Request) *viewerAccess {
	return &viewerAccess{
		viewer: viewerID(r),
		policy: s.policy,
		repo:   s.repo,
	}
}

// isAdmin reports whether this viewer may change the library: scan, enrich, add
// and remove libraries. Seeing a library and changing the library are separate
// grants, and this one grants nothing to look at.
func (a *viewerAccess) isAdmin() bool {
	return a.policy.IsAdmin(a.viewer)
}

// requireAdmin refuses a request that changes the library when the policy does
// not list the viewer as an admin, and reports whether the handler may carry on.
//
// With no policy every admitted viewer is an admin, which is what an install has
// done until now: the gate decided whether a request was allowed through, and
// nothing decided what it was allowed to do.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.access(r).isAdmin() {
		return true
	}
	writeError(w, http.StatusForbidden, "admin_required",
		"this changes the library, and the access policy does not list this viewer as an admin")
	return false
}

// names returns the library id-to-name map the policy matches against. With no
// policy there is nothing to match, so nothing is read.
func (a *viewerAccess) names(ctx context.Context) (map[string]string, error) {
	if a.policy == nil {
		return nil, nil
	}
	if a.libraryNames != nil {
		return a.libraryNames, nil
	}

	libraries, err := a.repo.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(libraries))
	for _, lib := range libraries {
		names[lib.ID] = lib.Name
	}
	a.libraryNames = names
	return names, nil
}

// allowed reports whether the viewer may see a library known only by its id.
func (a *viewerAccess) allowed(ctx context.Context, libraryID string) (bool, error) {
	if a.policy == nil {
		return true, nil
	}
	names, err := a.names(ctx)
	if err != nil {
		return false, err
	}
	return a.policy.AllowsLibrary(a.viewer, libraryID, names[libraryID]), nil
}

// libraries returns the libraries this viewer may see.
func (a *viewerAccess) libraries(ctx context.Context) ([]library.Library, error) {
	all, err := a.repo.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	if a.policy == nil {
		return all, nil
	}

	a.libraryNames = make(map[string]string, len(all))
	visible := make([]library.Library, 0, len(all))
	for _, lib := range all {
		a.libraryNames[lib.ID] = lib.Name
		if a.policy.AllowsLibrary(a.viewer, lib.ID, lib.Name) {
			visible = append(visible, lib)
		}
	}
	return visible, nil
}

// library returns one library, or library.ErrNotFound when it does not exist or
// the viewer may not see it. The two are reported identically on purpose: an
// API that answers "forbidden" for some ids and "not found" for others is a way
// to enumerate what the server holds.
func (a *viewerAccess) library(ctx context.Context, id string) (*library.Library, error) {
	lib, err := a.repo.GetLibrary(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.policy == nil {
		return lib, nil
	}
	if !a.policy.AllowsLibrary(a.viewer, lib.ID, lib.Name) {
		return nil, library.ErrNotFound
	}
	return lib, nil
}

// entities returns the entities of every library this viewer may see.
func (a *viewerAccess) entities(ctx context.Context) ([]library.MediaEntity, error) {
	all, err := a.repo.ListEntities(ctx)
	if err != nil {
		return nil, err
	}
	if a.policy == nil {
		return all, nil
	}

	names, err := a.names(ctx)
	if err != nil {
		return nil, err
	}
	visible := make([]library.MediaEntity, 0, len(all))
	for _, entity := range all {
		if a.policy.AllowsLibrary(a.viewer, entity.LibraryID, names[entity.LibraryID]) {
			visible = append(visible, entity)
		}
	}
	return visible, nil
}

// entitiesInLibrary returns the entities of one library, or ErrNotFound when
// the viewer may not see the library at all.
func (a *viewerAccess) entitiesInLibrary(ctx context.Context, libraryID string) ([]library.MediaEntity, error) {
	if _, err := a.library(ctx, libraryID); err != nil {
		return nil, err
	}
	return a.repo.ListEntitiesByLibrary(ctx, libraryID)
}

// entity returns one entity, or ErrNotFound when it does not exist or its
// library is one the viewer may not see.
func (a *viewerAccess) entity(ctx context.Context, id string) (*library.MediaEntity, error) {
	entity, err := a.repo.GetEntity(ctx, id)
	if err != nil {
		return nil, err
	}
	allowed, err := a.allowed(ctx, entity.LibraryID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, library.ErrNotFound
	}
	return entity, nil
}

// children returns the children of an entity the viewer may see. The check is
// the parent's, which is sufficient: a child lives in its parent's library, so
// walking down from a visible node cannot cross into a hidden one.
func (a *viewerAccess) children(ctx context.Context, parentID string) ([]library.MediaEntity, error) {
	if _, err := a.entity(ctx, parentID); err != nil {
		return nil, err
	}
	return a.repo.ListChildren(ctx, parentID)
}

// objects returns the media objects of an entity the viewer may see.
func (a *viewerAccess) objects(ctx context.Context, entityID string) ([]library.MediaObject, error) {
	if _, err := a.entity(ctx, entityID); err != nil {
		return nil, err
	}
	return a.repo.GetObjectsByEntity(ctx, entityID)
}

// object returns one media object, or ErrNotFound when it does not exist or its
// entity is out of sight. The file path lives on the object, so this is the
// check that stops a direct file URL from being a way around the listing.
func (a *viewerAccess) object(ctx context.Context, id string) (*library.MediaObject, error) {
	object, err := a.repo.GetObject(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := a.entity(ctx, object.MediaEntityID); err != nil {
		return nil, err
	}
	return object, nil
}
