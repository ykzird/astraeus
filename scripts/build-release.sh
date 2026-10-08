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
