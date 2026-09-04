---
title: Installation
weight: 1
description: Every way to install fpm-tune, how a release is verified, and where it lives on a host.
---

# Installation

This page is for the person putting fpm-tune on a host: each install method, how the release is verified, and which files it owns once it runs.

## The install script

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

The script detects the platform (Linux or macOS, amd64 or arm64), downloads the matching release archive, verifies it, and installs the binary into the first of `/usr/local/bin`, `~/.local/bin` and `~/bin` that exists and is writable. If none does, it creates `~/.local/bin`. It warns when the directory it picked is not on `PATH`.

It never runs `sudo`. On a server, run it as root so the binary lands in `/usr/local/bin`; installed as an ordinary user it lands in `~/.local/bin`, and `sudo fpm-tune` then fails because `sudo` does not search there. If the script cannot write the directory it prints the `sudo install -m 0755 …` command to run instead.

The script reads two environment variables:

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | FPM_TUNE_VERSION=v1.0.0 sh
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | FPM_TUNE_INSTALL_DIR=/opt/bin sh
```

`FPM_TUNE_VERSION` defaults to `latest`, which resolves to the newest stable release or, while there is none, to the highest release of any kind, pre-releases included.

### What it verifies

Every release publishes `SHA256SUMS` and `SHA256SUMS.cosign.bundle`, a keyless Sigstore signature over the checksum file made by the release workflow. The script downloads `SHA256SUMS` and checks the archive against it; a mismatch is refused. When `cosign` is on `PATH` it also verifies the bundle, pinned to this repository's release workflow at the version being installed, and refuses on failure. Without `cosign` it prints `note: verifying the checksum only, not who published it`. The checksum proves the download was not corrupted; the signature proves who published it.

## Homebrew

```bash
brew install cboxdk/tap/fpm-tune
```

The formula is generated from a published release and carries that release's checksums, which Homebrew verifies on install.

## A release archive by hand

Download the archive, `SHA256SUMS` and `SHA256SUMS.cosign.bundle` from the [releases page](https://github.com/cboxdk/fpm-tune/releases). Verify the signature, then the archive, then install:

```bash
cosign verify-blob \
  --bundle SHA256SUMS.cosign.bundle \
  --certificate-identity "https://github.com/cboxdk/fpm-tune/.github/workflows/release.yml@refs/tags/v1.0.0" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf fpm-tune-1.0.0-linux-amd64.tar.gz
sudo install -m 0755 fpm-tune-1.0.0-linux-amd64/fpm-tune /usr/local/bin/fpm-tune
```

Substitute the tag you downloaded. Keep `--certificate-identity`: without it cosign accepts any valid Sigstore signature, including one made by someone else. `python -m sigstore verify identity` with the same bundle, identity and issuer verifies it without cosign.

## go install

```bash
go install github.com/cboxdk/fpm-tune/cmd/fpm-tune@latest
```

This builds from source, so nothing is signed; the binary goes to `$GOBIN`, by default `~/go/bin`. In a clone, `make build` builds `build/fpm-tune`.

## Where it lives on a host

| path | what |
|---|---|
| `/usr/local/bin/fpm-tune` | the binary |
| `/etc/fpm-tune/config` | the service settings `install-service` writes and `serve --config` reads |
| `/etc/systemd/system/fpm-tune.service` | the unit `install-service` writes |
| `/var/lib/fpm-tune/state.json` | the learned baselines |
| `/var/lib/fpm-tune/backup/` | the previous configuration and the transaction record while a change is in flight, and a note of where php-fpm lives |
| `/var/lib/fpm-tune/recommended.conf` | the recommendation file, written by the installed advisory service |
| `/var/lib/fpm-tune/control.sock` | the socket `apply-now` and `top` ask the daemon on, mode 0600 |
| `/var/lib/fpm-tune/fpm-tune.lock` | the state lock: one writer of the state file at a time |
| `/run/fpm-tune/` | the per-pool-directory locks: one writer of a pool directory at a time |
| `zz-fpm-tune.conf` | the ceilings, in the pool directory the master includes (`/etc/php/8.4/fpm/pool.d` on Debian and Ubuntu, `/etc/php-fpm.d` elsewhere) |
| `zz-fpm-tune-status.conf` | the status pages, in the same directory |

`/var/lib/fpm-tune` is not scratch space. Deleting `backup/` while a change is in flight takes away the ability to undo it and to repair a host whose master will not start; see [Recovering](../operating/recovering.md).

## Checking it runs

```bash
fpm-tune version
fpm-tune plan
```

`version` prints `1.0.0`. `plan` reads the host and writes nothing. If it reports no pools, the cause is one of three:

1. A master is running but its pools have no status page. This is the usual case on a fresh host; the error names the pools and the fix, `sudo fpm-tune enable-status`.
2. No php-fpm master is running. `systemctl status php8.4-fpm` or `pgrep -a php-fpm` says so. The unit is `php8.4-fpm` on Debian and Ubuntu, which is what Forge and Ploi run, and `php-fpm` elsewhere.
3. A master is running but this process cannot see it: discovery reads the process table, and reading another user's processes needs root.

To run it in the background, see [Running as a daemon](../operating/running-as-a-daemon.md).
