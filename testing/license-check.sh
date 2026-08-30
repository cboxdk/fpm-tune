#!/usr/bin/env sh
#
# Fail the build if any dependency is offered under a license that is not
# permissive — reading the licenses cyclonedx-gomod already detected into
# sbom.json, so there is one source of truth and no second scan to drift.
#
# A component passes if ANY of its detected licenses is on the permissive
# allowlist (SPDX "OR" dual-licensing means one permissive option is enough),
# or if it is a documented exception below. A component with NO detected
# license fails: an unknown license is not a permissive one.
#
# The check reads the module graph, which is the same on every OS. The one
# exception below is not in the Linux server binary at all — see its note.
#
# Self-contained: jq and the committed SBOM, no network, no plugins.
set -eu

SBOM="${1:-sbom.json}"
command -v jq >/dev/null 2>&1 || { echo "license-check needs jq"; exit 2; }
[ -f "$SBOM" ] || { echo "no $SBOM — run 'make sbom' first"; exit 2; }

# The policy is entirely in jq, so the shell only has to ask whether the list of
# offenders is empty — no reading loop, no subshell swallowing the exit state.
#
#   $ok      permissive SPDX ids (a package passes on ANY of these)
#   $except  {module: license} allowed despite not being permissive, each with a
#            reason it is safe — kept here so the list and the justification
#            cannot drift apart.
#
# github.com/shoenig/go-m1cpu is MPL-2.0 (weak, file-level copyleft). It is a
# transitive dependency of gopsutil for Apple-Silicon CPU detection, compiled
# ONLY into darwin/arm64 builds — `GOOS=linux go list -deps ./...` never names
# it — so the Linux server binary ships none of it, and MPL-2.0 permits binary
# distribution regardless. Reviewed 2026-08.
offenders=$(jq -r '
  ["MIT","BSD-2-Clause","BSD-3-Clause","Apache-2.0","ISC","0BSD","Unlicense"] as $ok
  | {"github.com/shoenig/go-m1cpu": "MPL-2.0"} as $except
  | .components[]?
  | . as $c
  | ((.evidence.licenses // .licenses // [])
      | map(.license.id // .license.name // .expression)
      | map(select(. != null))) as $lics
  | ($lics | any(. as $l | $ok | index($l))) as $permissive
  | select($permissive | not)
  | select(($except[$c.name] // null) as $ex
      | ($ex == null) or ($lics | index($ex) | not))
  | "\($c.name): \(if ($lics | length) > 0 then ($lics | join(", ")) else "no license detected" end)"
' "$SBOM")

if [ -n "$offenders" ]; then
  echo "❌ dependencies under a non-permissive, undocumented license:"
  echo "$offenders" | sed 's/^/  /'
  echo
  echo "Add a documented exception in testing/license-check.sh only with a reason"
  echo "the license is safe to ship — otherwise replace the dependency."
  exit 1
fi

echo "✅ every dependency license is permissive or a documented exception"
