---
title: Installation
weight: 1
description: Install script, Homebrew, or from source — and how to verify what you got.
---

# Installation

## Install script

The primary channel. Works on macOS and Linux, including servers where Homebrew
does not belong.

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

It detects your platform, downloads the matching release, **verifies the SHA-256
checksum and refuses to install on a mismatch**, and places the binary in the first
writable directory on your `PATH`. When `cosign` is present it also verifies the
release's Sigstore signature, and says plainly when it is not — see
[Verifying a release](#verifying-a-release).

It never invokes `sudo`. If the target directory is not writable it prints the exact
command to run instead — a tool that silently escalates is one you cannot reason about.

Overrides:

```bash
FPM_TUNE_VERSION=0.1.0-beta.3 sh install.sh
FPM_TUNE_INSTALL_DIR="$HOME/.local/bin" sh install.sh
```

## Homebrew

```bash
brew install cboxdk/tap/fpm-tune
```

The formula is generated from a published release by `scripts/publish-formula.py`,
so it can only ever describe a build that exists. It carries the release's own
checksums and Homebrew verifies them on install.

## Verifying a release

Every release publishes `SHA256SUMS` and `SHA256SUMS.cosign.bundle`, a keyless
[Sigstore](https://www.sigstore.dev/) signature over it, in the standardised bundle
format that cosign 2.4+, `sigstore-python` and `sigstore-go` all read. There is no
public key to fetch or trust on first use: the signing identity is the release
workflow itself, attested by GitHub's OIDC provider and recorded in the public
transparency log.

`install.sh` does this automatically when `cosign` is on your `PATH`. To check by
hand, with cosign:

```bash
cosign verify-blob \
  --bundle SHA256SUMS.cosign.bundle \
  --certificate-identity "https://github.com/cboxdk/fpm-tune/.github/workflows/release.yml@refs/tags/v0.1.0-beta.3" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  SHA256SUMS
```

Then check the archive against the verified checksums:

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

Substitute the tag you actually downloaded for `v0.1.0-beta.3`. And **pin
`--certificate-identity`**: without it, cosign accepts any valid Sigstore signature —
including one made by someone else entirely. The identity is what ties the signature
to this repository's release workflow at that tag.

Or, without leaving Python and without a binary from another project's releases —
every fpm-tune release is signed into this bundle format from the first one, so this
works for all of them:

```bash
pipx install sigstore     # or: pip install sigstore
python -m sigstore verify identity \
  --bundle SHA256SUMS.cosign.bundle \
  --cert-identity "https://github.com/cboxdk/fpm-tune/.github/workflows/release.yml@refs/tags/v0.1.0-beta.3" \
  --cert-oidc-issuer "https://token.actions.githubusercontent.com" \
  SHA256SUMS
```

The checksum file alone proves your download was not corrupted in transit. It comes
from the same server as the archive, so on its own it says nothing about who produced
it; the signature is what does that.

## From source

```bash
git clone https://github.com/cboxdk/fpm-tune
cd fpm-tune
make build          # → build/fpm-tune
```

Or straight from the module path:

```bash
go install github.com/cboxdk/fpm-tune/cmd/fpm-tune@latest
```

The result is a single static binary with no runtime dependency beyond php-fpm
itself. It cross-compiles for `linux/amd64` and `linux/arm64` (and builds on
darwin, where the sizing logic runs but the host-reading does not).

## Where it lives on a host

- **The binary** goes wherever you keep operational tools — `/usr/local/bin` is
  conventional.
- **`/var/lib/fpm-tune`** holds the learned baselines (`state.json`) and, while a
  change is in flight, the previous configuration, the recovery record, and a
  note of where php-fpm lives. This directory is not scratch space: cleaning it
  removes the ability to undo a change and to repair a host whose master will not
  start. See [Recovering a host](../operating/recovering.md).
- **`zz-fpm-tune.conf`** is the one file it writes, into the directory your
  master already includes (`/etc/php-fpm.d` on RHEL, `/etc/php/*/fpm/pool.d` on
  Debian). It contains only `pm.*` keys; your pool configuration is not touched.

## As a systemd service

```ini
[Unit]
Description=fpm-tune
# Wants, not Requires: a supervisor that dies with the thing it supervises
# cannot repair it.
Wants=php-fpm.service
After=php-fpm.service

[Service]
ExecStart=/usr/local/bin/fpm-tune serve --apply --metrics 127.0.0.1:9110
Restart=on-failure
RestartSec=5

# systemd owns /var/lib/fpm-tune with sensible permissions, rather than the
# tool creating it under whatever umask it inherited.
StateDirectory=fpm-tune
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
```

Bind it with `Wants=`, not `Requires=` — a supervisor that dies with the thing it
supervises cannot repair it.

## Checking it runs

```bash
fpm-tune version    # prints the version it was built as
fpm-tune plan       # reads the host, writes nothing
```

If `plan` reports no pools found, either no php-fpm master is running or the tool
cannot see it — discovery reads the process table, and inspecting another user's
processes needs root. The error says which.

## Hand this to an agent

A self-contained brief. It names only commands that exist, and it stops at the
read-only step on purpose: fpm-tune is beta and writes production php-fpm
configuration, so a human reads the recommendation before anything is applied.

````markdown
# Task: install fpm-tune and show what it would recommend

fpm-tune is a single static binary. It reads a host's memory budget and the real
per-worker RSS of every php-fpm pool, and sizes `pm.max_children` to fit — reloading
php-fpm, never restarting it. No runtime dependency beyond php-fpm itself.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

The installer verifies the release checksum and refuses to install on a mismatch. On
macOS, `brew install cboxdk/tap/fpm-tune` is equivalent.

## Show the recommendation — read-only

```bash
fpm-tune version
fpm-tune plan
```

`plan` reads the process table and each pool's status and prints what it would set,
with the reasoning. **It writes nothing.** If it reports no pools, either no php-fpm
master is running or discovery cannot see it without root — the error says which.

## Do not

- **Do not pass `--apply`.** That writes a drop-in and reloads php-fpm. It is a
  deliberate, human-reviewed step, not something to run unprompted — read what `plan`
  recommends first.
- Do not delete `/var/lib/fpm-tune`; it holds the learned baselines and the record
  needed to undo a change.
````
