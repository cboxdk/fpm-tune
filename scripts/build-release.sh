#!/bin/sh
# Build the release archives and their checksums into dist/.
#
#   scripts/build-release.sh 0.1.0-beta.1
#
# One script so a local `make dist` and the release workflow build the archives the
# same way — the installer and the Homebrew formula both depend on the exact names
# and layout produced here, and a second copy of this logic is a second thing to
# drift.
#
# The binaries are fully static (CGO disabled), so one Linux archive runs on Alpine
# and Debian alike; the "musl" in the target name is the tap's convention, not a
# libc dependency.

set -eu

VERSION="${1:?usage: build-release.sh <version>}"
VERSION="${VERSION#v}"

BIN="fpm-tune"
DIST="dist"

rm -rf "$DIST"
mkdir -p "$DIST"

# target triple -> "GOOS GOARCH"
build_one() {
    triple="$1"
    goos="$2"
    goarch="$3"

    name="$BIN-$VERSION-$triple"
    out="$DIST/$name"
    mkdir -p "$out"

    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
        -o "$out/$BIN" ./cmd/fpm-tune

    # Fully static: refuse to ship a Linux binary that turned out to be dynamically
    # linked, which would fail on exactly the minimal images this is meant for.
    if [ "$goos" = "linux" ] && command -v file >/dev/null 2>&1; then
        file "$out/$BIN" | grep -q "statically linked" \
            || { echo "error: $triple is not statically linked" >&2; exit 1; }
    fi

    cp README.md LICENSE SECURITY.md "$out/" 2>/dev/null || true
    [ -d docs ] && cp -R docs "$out/"

    tar -czf "$DIST/$name.tar.gz" -C "$DIST" "$name"
    rm -rf "$out"
}

build_one x86_64-unknown-linux-musl  linux  amd64
build_one aarch64-unknown-linux-musl linux  arm64
build_one x86_64-apple-darwin        darwin amd64
build_one aarch64-apple-darwin       darwin arm64

# One checksum file over every archive, sorted by name so it is stable across builds.
( cd "$DIST" && for f in *.tar.gz; do
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$f"; else shasum -a 256 "$f"; fi
  done | sort -k2 > SHA256SUMS )

echo "built into $DIST/:"
ls -1 "$DIST"
