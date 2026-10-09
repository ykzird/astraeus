package api

import (
	"net/http"
	"strings"
)

// uiFileServer serves the web directory without exposing what is in it.
//
// http.FileServer on its own is a directory browser: `GET /vendor/` answers with
// a listing of everything in the directory, and any dotfile in the tree is served
// to anyone who names it. Neither is what a UI bundle needs, and both hand a
// stranger a map of the install (A-10 of the 2026-10-09 review). The UI is a fixed
// set of files the build produces, so the server knows what it means to serve and
// does not need to answer the question "what else is there".
//
// Two refusals:
//
//   - **A path segment beginning with a dot.** `.version`, `.env`, `.git` - a
//     dotfile in a served tree is configuration or version control, never an
//     asset the UI asks for.
//   - **A path ending in a slash.** That is a request for a directory, and
//     http.FileServer answers it with a listing when there is no index file. The
//     UI never asks for one - every asset it wants has a file name, and the root
//     is fetched as `/` which http.FileServer resolves to index.html through the
//     redirect below rather than through this handler.
//
// Symlinks are followed, deliberately: `web/icons` and `web/vendor` are symlinked
// to the directories that own them in this repository, so refusing to follow them
// would stop the UI working in a checkout. They are excluded from a release
// archive - see scripts/build-release.sh - where the files are real.
func uiFileServer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uiPathIsHidden(r.URL.Path) || uiPathIsDirectory(r.URL.Path) {
			// 404 rather than 403: the point is not that the path is forbidden but
			// that there is no such file, and saying "forbidden" confirms that
			// there is something there to forbid.
			writeError(w, http.StatusNotFound, "not_found", "no such file")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// uiPathIsHidden reports whether any segment of a request path begins with a dot.
//
// The check is per segment rather than on the whole path so `/.env` and
// `/vendor/.version` are both caught, and on the raw path rather than a cleaned
// one so nothing is normalised out of the way first. A percent-encoded dot is
// already decoded by the time net/http hands the request over.
func uiPathIsHidden(requestPath string) bool {
	for _, segment := range strings.Split(requestPath, "/") {
		if strings.HasPrefix(segment, ".") && segment != "." && segment != ".." {
			return true
		}
	}
	return false
}

// uiPathIsDirectory reports whether a request path asks for a directory.
//
// The root is not one of them. `/` is how a browser asks for the UI, and
// http.FileServer answers it by serving index.html - so treating it as a
// directory request would refuse the one path every viewer uses. What is refused
// is a *named* directory: `/vendor/`, `/icons/`, the shape of a listing.
func uiPathIsDirectory(requestPath string) bool {
	return requestPath != "/" && strings.HasSuffix(requestPath, "/")
}
