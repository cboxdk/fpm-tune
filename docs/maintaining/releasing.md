---
title: Releasing
weight: 51
description: What a release does, what it signs, and the one secret it needs to finish the job.
---

# Releasing

A release is one tag push. Nothing else needs bumping: the version is stamped into
the binary from the tag, and the Homebrew formula's version is rewritten from the tag
when it is published — so there is no second file to keep in sync and forget.

```bash
git tag -a v0.1.0-beta.3 -m "…" && git push origin v0.1.0-beta.3
```

A tag with a pre-release suffix (`-beta.3`, `-rc.1`) is published as a pre-release, so
it is not what `brew install` or the installer's `latest` resolves to.

## What the release workflow does

On a `v*` tag, [`release.yml`](https://github.com/cboxdk/fpm-tune/blob/main/.github/workflows/release.yml):

1. Builds all four archives with `scripts/build-release.sh` — the same script `make
   dist` runs, so a local build and this one produce byte-for-byte the same archives.
   The installer and the formula depend on the exact names and layout it emits. The
   Linux targets are fully static, so one binary runs on Alpine and Debian alike.
2. Signs `SHA256SUMS` with a keyless [Sigstore](https://www.sigstore.dev/) signature.
   The signer is the workflow's own OIDC identity, so there is no private key to
   store, rotate, or leak — one signature over the checksum file covers every archive.
3. Publishes the archives, the checksum file, and the signature bundle as a GitHub
   release, with generated notes.

That is the whole release. It does not touch the Homebrew tap.

## Why the tap is a separate workflow

The tap is updated by [`formula.yml`](https://github.com/cboxdk/fpm-tune/blob/main/.github/workflows/formula.yml),
which runs **after** the release workflow finishes.

`scripts/publish-formula.py` reads the digests out of the *published* `SHA256SUMS`, so
it cannot run before the release exists. Splitting it out also bounds the damage: a
tap that cannot be written to costs nothing worse than Homebrew serving the previous
version until the workflow is re-run, and re-running it is a single
`workflow_dispatch`.

It triggers on the release workflow *completing*, not on `release: published` —
because the release is created with `GITHUB_TOKEN`, and events raised by
`GITHUB_TOKEN` do not trigger further workflows, so `release: published` would never
fire for an automated release. (That trigger is kept anyway, for a release published
by hand, which does fire it.)

## The credential

The tap is a different repository, so `GITHUB_TOKEN` cannot write to it.

A **GitHub App** provides the credential, not a personal access token. An App's
credentials do not expire, so nothing stops working on a date nobody wrote down, and
it does not belong to a person — a PAT leaves with whoever created it. What reaches
the runner is an installation token scoped to `homebrew-tap` alone that expires an
hour later.

Set up once: an App named **cbox-tap-publisher** under the `cboxdk` organisation, with
**Contents: read and write** and no webhook, installed on the org, its client id in
`formula.yml`, and its private key stored as a secret:

```bash
gh secret set HOMEBREW_APP_PRIVATE_KEY --repo cboxdk/fpm-tune < cbox-tap-publisher.private-key.pem
```

That is the only secret involved. The client id identifies the App rather than
authenticating as it — any organisation member can read it back from
`/orgs/cboxdk/installations` — so it lives in the workflow, where it also documents
which App is doing the publishing. The private key goes in as the whole PEM, header
and footer included — hence `<` a file rather than a paste. (The same App and secret
serve every cboxdk repo that publishes to the tap; promote it to an org secret to
share one copy.)

**Without it the release still succeeds.** `formula.yml` warns and prints the command
to run by hand:

```bash
python3 scripts/publish-formula.py v0.1.0-beta.3
```

That is deliberate. A missing tap update should not fail a release that is otherwise
good, but it must not pass silently either: a tap that has quietly stopped being
updated is indistinguishable from one that is current, and `brew install` would go on
serving the previous version.

## What the publisher refuses to do

`scripts/publish-formula.py` will not publish if a checksum is missing from the
release, if the archives are listed in an order that would pair digests with the wrong
targets, or if a placeholder or a `#{version}` interpolation survives substitution. A
tap serving a wrong digest fails on someone else's machine, which is the worst place
to find out.

## Re-publishing to the tap by hand

`workflow_dispatch` on `formula.yml` checks out the **tag** you give it and runs *that
tag's* copy of `publish-formula.py`. That is correct for any recent tag, but re-running
it against an old tag runs the publisher as it was then — before any later fix to it.
To republish an older release with the current publisher, run it from a current
checkout instead:

```bash
python3 scripts/publish-formula.py v0.1.0-beta.3
```

It reads the release's own `SHA256SUMS`, so it works for any tag that has a published
release.
