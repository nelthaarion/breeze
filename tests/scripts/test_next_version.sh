#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NEXT_VERSION_SCRIPT="$SCRIPT_DIR/../../scripts/next-version.sh"

fail() {
  echo "FAIL: $1"
  exit 1
}

# Test 1: picks the highest valid semver tag and bumps the patch segment,
# ignoring malformed tags that exist in this repo's real history.
tmp1=$(mktemp -d)
git -C "$tmp1" init -q
git -C "$tmp1" config user.email "test@example.com"
git -C "$tmp1" config user.name "Test"
git -C "$tmp1" commit -q --allow-empty -m "init"
git -C "$tmp1" tag v1.5.1
git -C "$tmp1" tag v1.5.2
git -C "$tmp1" tag "v.1.4.1"
git -C "$tmp1" tag "1.0.1"
git -C "$tmp1" tag "v1.0"
result=$(cd "$tmp1" && bash "$NEXT_VERSION_SCRIPT")
[ "$result" = "v1.5.3" ] || fail "expected v1.5.3, got $result"
rm -rf "$tmp1"

# Test 2: falls back to v0.1.0 when no matching tags exist at all.
tmp2=$(mktemp -d)
git -C "$tmp2" init -q
git -C "$tmp2" config user.email "test@example.com"
git -C "$tmp2" config user.name "Test"
git -C "$tmp2" commit -q --allow-empty -m "init"
result=$(cd "$tmp2" && bash "$NEXT_VERSION_SCRIPT")
[ "$result" = "v0.1.0" ] || fail "expected v0.1.0, got $result"
rm -rf "$tmp2"

echo "All next-version tests passed."
