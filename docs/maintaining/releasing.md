---
title: Releasing
weight: 1
description: What a tag push triggers, what gets signed, how the Homebrew tap is updated, and the one secret involved.
---

# Releasing

This page is for a maintainer cutting a release. A release is one tag push; the workflows do the rest.

## The flow

Changes reach `main` by squash-merged pull requests. The version is stamped into the binary from the tag and the Homebrew formula's version is rewritten from the tag when it is published, so nothing else needs bumping. Push the tag:

```bash
git tag -a v1.0.0 -m "v1.0.0" && git push origin v1.0.0
```

Do not run `gh release create` yourself. The Release workflow creates the release, and if one already exists for the tag the workflow fails with "already exists" and publishes nothing.

Docs ship only in tags: the docs site reads `docs/` from tagged releases, and the archives carry a copy of `docs/`. A fix on `main` is not published until the next tag.

## What the Release workflow does

On a `v*` tag, [`release.yml`](https://github.com/cboxdk/fpm-tune/blob/main/.github/workflows/release.yml):

1. Builds the four archives (linux and darwin, amd64 and arm64) with `scripts/build-release.sh`, the same script `make dist` runs, so a local build and this one produce the same archives. The Linux binaries are fully static, and the script refuses to ship one that is not. Each archive carries the binary, `README.md`, `LICENSE`, `SECURITY.md` and `docs/`.
2. Signs `SHA256SUMS` with a keyless [Sigstore](https://www.sigstore.dev/) signature via `cosign`. The signer is the workflow's own OIDC identity, so there is no private key to store or rotate, and one signature over the checksum file covers every archive.
3. Runs `gh release create` with the archives, `SHA256SUMS` and the signature bundle, with generated notes. A tag with a suffix (`-rc.1`, `-beta.1`) is created as a pre-release.

It does not touch the Homebrew tap.

## Versions and what "latest" means

The version is semantic. Within a major, the commands, flags, config keys, drop-in names, metric names and `/history.json` fields are stable; a release that breaks one of them bumps the major, and anything added is a minor. A tag with a suffix (`v1.1.0-rc.1`) is published as a pre-release, which GitHub's `releases/latest` does not return; `install.sh` then keeps resolving `latest` to the newest stable release, and the pre-release is reachable only with `FPM_TUNE_VERSION`. (Before 1.0 there was no stable release, and `latest` fell back to the newest beta.)

## The Homebrew formula

The tap is updated by [`formula.yml`](https://github.com/cboxdk/fpm-tune/blob/main/.github/workflows/formula.yml), which runs when the Release workflow completes. `scripts/publish-formula.py` reads the digests from the published `SHA256SUMS`, so it cannot run before the release exists. Keeping it separate also bounds the damage: a tap that cannot be written costs nothing worse than Homebrew serving the previous version until the workflow is re-run, and re-running is one `workflow_dispatch`.

It triggers on the Release workflow completing rather than on `release: published`, because the release is created with `GITHUB_TOKEN`, and events raised by `GITHUB_TOKEN` do not trigger other workflows. The `release: published` trigger is kept for a release published by hand, which does fire it.

## The credential

The tap is a different repository, so `GITHUB_TOKEN` cannot write to it. A GitHub App provides the credential: its credentials do not expire and do not belong to a person, and what reaches the runner is an installation token scoped to `homebrew-tap` alone that expires an hour later.

Set up once: an App named **cbox-tap-publisher** under the `cboxdk` organisation, with **Contents: read and write** and no webhook, installed on the organisation, its client id in `formula.yml`, and its private key stored as a secret:

```bash
gh secret set HOMEBREW_APP_PRIVATE_KEY --repo cboxdk/fpm-tune < cbox-tap-publisher.private-key.pem
```

That is the only secret involved. The client id identifies the App rather than authenticating as it, so it lives in the workflow. The private key goes in as the whole PEM, header and footer included, hence `<` a file rather than a paste. The same App and secret can serve every cboxdk repository that publishes to the tap; promote it to an organisation secret to share one copy.

Without the secret the release still succeeds. `formula.yml` warns and prints the command to run by hand:

```bash
python3 scripts/publish-formula.py v1.0.0
```

A missing tap update should not fail a release that is otherwise good, and it must not pass silently either, because a tap that has stopped updating looks like one that is current.

## What the publisher refuses

`scripts/publish-formula.py` will not publish if a checksum is missing from the release, if the archives are listed in an order that would pair digests with the wrong targets, or if a placeholder or a `#{version}` interpolation survives substitution. A tap serving a wrong digest fails on someone else's machine.

## Re-publishing by hand

`workflow_dispatch` on `formula.yml` checks out the tag you give it and runs that tag's copy of `publish-formula.py`. For a recent tag that is right; for an old tag it runs the publisher as it was then. To republish an older release with the current publisher, run it from a current checkout:

```bash
python3 scripts/publish-formula.py v1.0.0
```

It reads the release's own `SHA256SUMS`, so it works for any tag with a published release.
