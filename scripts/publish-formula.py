#!/usr/bin/env python3
"""Fill in a release's checksums and publish the Homebrew formula to the tap.

The formula in `packaging/` carries placeholder checksums on purpose: it is
versioned with the source, and the digests only exist once a release has been
built. This reads them from the published `SHA256SUMS`, substitutes them, and
pushes the result to cboxdk/homebrew-tap.

Refuses rather than guesses. If a checksum is missing, or a placeholder survives
substitution, nothing is published — a tap that serves a formula with the wrong
digest fails at install time on the user's machine, the worst place to find out.

    python3 scripts/publish-formula.py v0.1.0-beta.1
    python3 scripts/publish-formula.py v0.1.0-beta.1 --dry-run
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

REPO = "cboxdk/fpm-tune"
TAP = "cboxdk/homebrew-tap"
PLACEHOLDER = "REPLACED_BY_RELEASE_WORKFLOW"

# The order the formula lists them, so a mismatch is a substitution bug rather than
# a silently wrong pairing.
TARGETS = [
    "aarch64-apple-darwin",
    "x86_64-apple-darwin",
    "aarch64-unknown-linux-musl",
    "x86_64-unknown-linux-musl",
]


def run_or_explain(command: list[str], what: str) -> None:
    """Run a command; on failure say what it PRINTED, not only what it was."""
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if result.returncode == 0:
        return
    detail = (result.stderr or result.stdout or "").strip() or "no output"
    sys.exit(f"{what} failed (exit {result.returncode}): {detail}")


def checksums(tag: str) -> dict[str, str]:
    url = f"https://github.com/{REPO}/releases/download/{tag}/SHA256SUMS"
    with urllib.request.urlopen(url, timeout=60) as response:
        text = response.read().decode()

    digests: dict[str, str] = {}
    for line in text.splitlines():
        parts = line.split()
        if len(parts) != 2:
            continue
        digest, name = parts
        for target in TARGETS:
            if name.endswith(f"-{target}.tar.gz"):
                digests[target] = digest
    return digests


def render(formula: str, version: str, digests: dict[str, str]) -> str:
    # Placeholders are substituted positionally, so a formula listing its archives
    # in a different order would pair every checksum with the wrong target — and
    # Homebrew would only find out on someone else's machine. Check the order first.
    order = re.findall(r"fpm-tune-#\{version\}-([a-z0-9_]+-[a-z0-9-]+)\.tar\.gz", formula)
    if order != TARGETS:
        sys.exit(f"the formula lists targets in an unexpected order: {order}")

    out = formula
    for target in TARGETS:
        digest = digests.get(target)
        if not digest:
            sys.exit(f"no checksum published for {target}; refusing to publish")
        out = out.replace(PLACEHOLDER, digest, 1)

    if PLACEHOLDER in out:
        sys.exit("a placeholder survived substitution; refusing to publish")

    # Resolve #{version} into the published formula. The version stanza stays: it is
    # what the URLs interpolate, and a prerelease version is not always parseable
    # from the URL by Homebrew.
    out = out.replace("#{version}", version)
    if "#{version}" in out:
        sys.exit("a version interpolation did not resolve; refusing to publish")

    # The tag is the source of truth for the version, not the template's stanza:
    # rewrite it so cutting a release never depends on remembering to bump a second
    # file. The template still carries a version so it reads as a real formula and
    # so #{version} above has a value when someone renders it by hand.
    out = re.sub(r'^(\s*)version "[^"]+"', rf'\1version "{version}"', out, count=1, flags=re.M)
    if f'version "{version}"' not in out:
        sys.exit("the version stanza did not resolve; refusing to publish")
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag", help="the release tag, e.g. v0.1.0-beta.1")
    parser.add_argument("--dry-run", action="store_true", help="print, do not push")
    args = parser.parse_args()

    version = args.tag.lstrip("v")
    source = Path(__file__).resolve().parent.parent / "packaging" / "fpm-tune.rb"
    formula = source.read_text(encoding="utf-8")

    rendered = render(formula, version, checksums(args.tag))

    if args.dry_run:
        print(rendered)
        return 0

    with tempfile.TemporaryDirectory() as workspace:
        run_or_explain(
            ["gh", "repo", "clone", TAP, workspace, "--", "--depth", "1"],
            f"cloning {TAP}",
        )
        formula_dir = Path(workspace) / "Formula"
        formula_dir.mkdir(exist_ok=True)
        (formula_dir / "fpm-tune.rb").write_text(rendered, encoding="utf-8")

        run_or_explain(["git", "-C", workspace, "add", "Formula/fpm-tune.rb"],
                       "staging the formula")
        status = subprocess.run(
            ["git", "status", "--porcelain"], cwd=workspace, capture_output=True, text=True
        )
        if not status.stdout.strip():
            print(f"the tap already serves {version}; nothing to do")
            return 0

        subprocess.run(
            ["git", "-c", "user.email=sn@cbox.dk", "-c", "user.name=Cbox",
             "commit", "-m", f"fpm-tune {version}"],
            cwd=workspace, check=True,
        )
        run_or_explain(["git", "-C", workspace, "push", "-u", "origin", "HEAD"],
                       f"pushing the formula to {TAP}")

    print(f"published fpm-tune {version} to {TAP}")
    print(f"verify with: brew install {TAP.replace('homebrew-', '')}/fpm-tune")
    return 0


if __name__ == "__main__":
    sys.exit(main())
