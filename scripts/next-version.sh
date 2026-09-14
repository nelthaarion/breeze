#!/usr/bin/env bash
set -euo pipefail

# `|| true` is required: with `pipefail`, grep exits non-zero when no tag matches,
# which would abort the script under `set -e` before the fallback below can run.
latest=$(git tag --list | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1 || true)

if [ -z "$latest" ]; then
  echo "v0.1.0"
  exit 0
fi

version=${latest#v}
major=$(echo "$version" | cut -d. -f1)
minor=$(echo "$version" | cut -d. -f2)
patch=$(echo "$version" | cut -d. -f3)
patch=$((patch + 1))

echo "v${major}.${minor}.${patch}"
