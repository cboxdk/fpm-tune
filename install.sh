#!/bin/sh
# Install fpm-tune.
#
#   curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
#
# POSIX sh, not bash: this has to run on an Alpine container and a stock Debian
# image as well as on a Mac. No sudo is invoked — if the install directory is not
# writable the script says exactly what to run instead, rather than escalating on
# its own.

set -eu

REPO="cboxdk/fpm-tune"
BIN="fpm-tune"

VERSION="${FPM_TUNE_VERSION:-latest}"
INSTALL_DIR="${FPM_TUNE_INSTALL_DIR:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "this installer needs $1"
}

detect_target() {
    os="$(uname -s)"
    arch="$(uname -m)"

    case "$os" in
        Linux)  os_part="linux" ;;
        Darwin) os_part="darwin" ;;
        *) die "unsupported operating system: $os. Build from source: https://github.com/$REPO" ;;
    esac

    case "$arch" in
        x86_64|amd64)  arch_part="amd64" ;;
        aarch64|arm64) arch_part="arm64" ;;
        *) die "unsupported architecture: $arch. Build from source: https://github.com/$REPO" ;;
    esac

    # GOOS-GOARCH, matching the archive names build-release.sh emits.
    printf '%s-%s' "$os_part" "$arch_part"
}

# First writable directory already on PATH, so the binary is runnable without the
# user editing their shell profile.
detect_install_dir() {
    if [ -n "$INSTALL_DIR" ]; then
        printf '%s' "$INSTALL_DIR"
        return
    fi
    for candidate in /usr/local/bin "$HOME/.local/bin" "$HOME/bin"; do
        if [ -d "$candidate" ] && [ -w "$candidate" ]; then
            printf '%s' "$candidate"
            return
        fi
    done
    # Nothing writable exists yet; ~/.local/bin is the least surprising to create.
    printf '%s' "$HOME/.local/bin"
}

resolve_version() {
    if [ "$VERSION" != "latest" ]; then
        printf '%s' "${VERSION#v}"
        return
    fi
    # The latest STABLE release first. While fpm-tune is in beta there is none, so
    # this 404s and we fall back to the newest release of any kind — including the
    # prerelease — which is what "latest" should mean before 1.0.
    tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n 1)"
    if [ -z "$tag" ]; then
        tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" 2>/dev/null \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
            | head -n 1)"
    fi
    [ -n "$tag" ] || die "could not determine the latest release; set FPM_TUNE_VERSION"
    printf '%s' "${tag#v}"
}

main() {
    need curl
    need tar

    target="$(detect_target)"
    version="$(resolve_version)"
    install_dir="$(detect_install_dir)"

    name="$BIN-$version-$target"
    url="https://github.com/$REPO/releases/download/v$version/$name.tar.gz"

    say "fpm-tune $version ($target)"

    tmp="$(mktemp -d)"
    # Clean up even if the download fails, so a retry does not accumulate temp dirs.
    trap 'rm -rf "$tmp"' EXIT INT TERM

    say "downloading $url"
    curl -fsSL "$url" -o "$tmp/$name.tar.gz" \
        || die "download failed. Check that v$version exists: https://github.com/$REPO/releases"

    # Verify against the release's checksum file. A failure here is refused rather
    # than warned about: an installer that continues past a bad checksum provides no
    # protection at all.
    if curl -fsSL "https://github.com/$REPO/releases/download/v$version/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
        # The checksum file arrives from the same server as the archive, so on its
        # own it proves the download was not corrupted — not that we published it.
        # A Sigstore signature over it is what establishes that, so verify it when
        # both cosign and the signature are present, and say plainly when they are
        # not.
        if command -v cosign >/dev/null 2>&1 &&
           curl -fsSL "https://github.com/$REPO/releases/download/v$version/SHA256SUMS.cosign.bundle" \
                -o "$tmp/SHA256SUMS.cosign.bundle" 2>/dev/null; then
            cosign verify-blob \
                --bundle "$tmp/SHA256SUMS.cosign.bundle" \
                --certificate-identity "https://github.com/$REPO/.github/workflows/release.yml@refs/tags/v$version" \
                --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
                "$tmp/SHA256SUMS" >/dev/null 2>&1 \
                || die "signature verification failed for v$version — refusing to install."
            say "signature verified"
        else
            say "note: verifying the checksum only, not who published it"
        fi

        expected="$(grep " $name.tar.gz\$" "$tmp/SHA256SUMS" | awk '{print $1}' | head -n 1)"
        if [ -n "$expected" ]; then
            if command -v sha256sum >/dev/null 2>&1; then
                actual="$(sha256sum "$tmp/$name.tar.gz" | awk '{print $1}')"
            elif command -v shasum >/dev/null 2>&1; then
                actual="$(shasum -a 256 "$tmp/$name.tar.gz" | awk '{print $1}')"
            else
                actual=""
                say "warning: no sha256 tool found, skipping checksum verification"
            fi
            if [ -n "$actual" ]; then
                [ "$actual" = "$expected" ] || die "checksum mismatch — refusing to install
  expected $expected
  actual   $actual"
                say "checksum verified"
            fi
        fi
    else
        say "warning: no SHA256SUMS published for v$version, skipping verification"
    fi

    tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
    [ -f "$tmp/$name/$BIN" ] || die "the archive did not contain $BIN"

    mkdir -p "$install_dir" 2>/dev/null || true
    if [ ! -w "$install_dir" ]; then
        die "$install_dir is not writable. Either:
  sudo install -m 0755 $tmp/$name/$BIN $install_dir/$BIN
or choose somewhere you own:
  FPM_TUNE_INSTALL_DIR=\$HOME/.local/bin sh install.sh"
    fi

    install -m 0755 "$tmp/$name/$BIN" "$install_dir/$BIN"
    say "installed $install_dir/$BIN"

    case ":$PATH:" in
        *":$install_dir:"*) ;;
        *) say ""
           say "note: $install_dir is not on your PATH. Add it with:"
           say "  export PATH=\"$install_dir:\$PATH\"" ;;
    esac

    say ""
    say "fpm-tune manages a running PHP-FPM. It reads the master's cgroup limit, sizes"
    say "each pool, and reloads without restarting. Start advisory-first — it changes"
    say "nothing until you add --apply:"
    say ""
    say "  $BIN plan               # what it would set, and why"
    say "  $BIN serve              # keep watching; add --apply to act"
    say ""
    say "On a server, run it as a service once you trust its recommendations:"
    say "  $BIN serve --apply --metrics 127.0.0.1:9110"
}

main "$@"
