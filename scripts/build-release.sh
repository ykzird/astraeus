#!/usr/bin/env bash
#
# Build the release artifacts for one version: a tar.gz per platform, holding
# the binary and the web UI it serves, plus a checksums file.
#
# The release workflow runs exactly this, and so can you. That is the point:
# every step of a release is runnable by hand, which is how it was verified
# before the repository had a remote.
#
# Usage:
#   scripts/build-release.sh <version> [goos/goarch ...]
#
#   scripts/build-release.sh 0.17.0
#   scripts/build-release.sh 0.17.0 linux/amd64 linux/arm64 darwin/arm64
#
# <version> is what `astraeus-server version` prints: the tag without its
# leading "v" (a v0.17.0 tag releases 0.17.0).

set -euo pipefail

version="${1:-}"
if [[ -z "$version" ]]; then
	echo "usage: $0 <version> [goos/goarch ...]" >&2
	exit 2
fi
shift

# Catching this here is worth it: a leading "v" would end up in the binary's
# own version string and in every filename.
if [[ "$version" == v* ]]; then
	echo "build-release: pass the version without the leading 'v' (got '$version')" >&2
	exit 2
fi

# CI has go on PATH; this development host reaches it through mise.
if ! command -v go >/dev/null 2>&1; then
	echo "build-release: go is not on PATH (here, run: mise exec -- $0 $*)" >&2
	exit 127
fi

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dist="$repo/dist"

platforms=("$@")
if [[ ${#platforms[@]} -eq 0 ]]; then
	platforms=("linux/amd64" "linux/arm64")
fi

cd "$repo"
rm -rf "$dist"
mkdir -p "$dist"

for platform in "${platforms[@]}"; do
	goos="${platform%%/*}"
	goarch="${platform##*/}"
	if [[ "$goos" == "$platform" || -z "$goarch" ]]; then
		echo "build-release: '$platform' is not goos/goarch" >&2
		exit 2
	fi

	name="astraeus-server_${version}_${goos}_${goarch}"
	stage="$dist/$name"
	mkdir -p "$stage"

	echo "building $name"
	# CGO_ENABLED=0 is what makes the cross-build need no C toolchain, and
	# -trimpath keeps the paths of this machine out of a published binary.
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
		-trimpath \
		-ldflags "-s -w -X main.version=$version" \
		-o "$stage/astraeus-server" ./cmd/astraeus-server

	# The binary serves the UI from --web-dir, so a release without web/ is only
	# half a server. The licence and the notices travel with it too.
	cp -R web "$stage/web"
	cp LICENSE THIRD_PARTY_NOTICES.md "$stage/"

	# deploy/README.md's systemd runbook is written to be followed from an
	# extracted archive: it installs the unit out of deploy/ and copies the
	# documents into /usr/local/share/doc/astraeus. Every path it names has to be
	# in here, or the runbook fails on a host that has nothing but the release.
	cp README.md SPECIFICATION.md TODO.md CONTEXT.md CONTRIBUTING.md SECURITY.md "$stage/"
	cp -R deploy "$stage/"

	# The README's header image is a relative path, so a reader of the installed
	# README needs the file beside it. Shipping the README without its assets
	# leaves a broken image in the one document a new user opens first.
	cp -R assets "$stage/"

	# Of docs/, ship only the pages a reader of the installed README needs to use
	# the thing. The repository's own working documents - handoff.md, and the
	# reviews - are about building it, not using it, and installing an internal
	# audit onto a user's host is noise they did not ask for.
	mkdir -p "$stage/docs"
	cp docs/index.md docs/playback.md docs/configuration.md docs/api.md \
		docs/development.md docs/troubleshooting.md "$stage/docs/"

	# That runbook is this layout's acceptance test, so run it here. A missing
	# file is a broken release, and this is the last point at which the build
	# can say so rather than a user on a clean machine.
	for f in astraeus-server web README.md SPECIFICATION.md TODO.md CONTEXT.md \
		CONTRIBUTING.md SECURITY.md LICENSE THIRD_PARTY_NOTICES.md deploy \
		deploy/astraeus.service docs/index.md docs/playback.md \
		docs/configuration.md docs/api.md docs/development.md \
		docs/troubleshooting.md \
		assets/astraeus.png; do
		[[ -e "$stage/$f" ]] || {
			echo "build-release: $f is missing from $name, so the runbook in deploy/README.md cannot be followed from this archive" >&2
			exit 1
		}
	done

	# And the working documents must not travel with a release.
	for f in docs/handoff.md docs/adversarial-review.md docs/review; do
		if [[ -e "$stage/$f" ]]; then
			echo "build-release: $f must not ship in $name" >&2
			exit 1
		fi
	done

	tar -C "$dist" -czf "$dist/$name.tar.gz" "$name"
	rm -rf "$stage"
done

# Sorted by name with relative paths, so the same sources produce the same file
# anywhere and `sha256sum -c` works from inside dist/.
(
	cd "$dist"
	sha256sum ./*.tar.gz | sed 's|\./||' | LC_ALL=C sort -k2 > checksums.txt
)

echo
echo "wrote:"
ls -1 "$dist"
echo
echo "verify with: (cd dist && sha256sum -c checksums.txt)"
