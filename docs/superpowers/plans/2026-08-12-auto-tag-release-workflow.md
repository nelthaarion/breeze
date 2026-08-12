# Auto-tag & Release Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a GitHub Actions workflow that automatically cuts a patch-bumped semver tag and GitHub Release on every merge to `main`, so `techcoach-breeze` can pin to stable tags instead of manual tagging.

**Architecture:** Version-bump logic lives in a standalone, testable shell script (`scripts/next-version.sh`) so it can be verified locally without invoking GitHub Actions. A new workflow (`.github/workflows/release.yml`) calls that script, then tags, pushes, and creates a release. A `workflow_dispatch` input (`dry_run`, default `true`) lets the workflow be exercised on a branch — computing the next version and printing it — without mutating tags or creating a release, which is how this plan verifies the workflow end-to-end before it ever runs for real on `main`.

**Tech Stack:** Bash, GitHub Actions YAML, GitHub CLI (`gh`).

## Global Constraints

- Only tags matching `^v[0-9]+\.[0-9]+\.[0-9]+$` count when finding the "latest" version — this repo's tag history has malformed entries (`v.1.4.1`, bare `1.0.1`, `v1.0`) that must be excluded.
- Auto-tagging only ever bumps the **patch** segment. Minor/major bumps stay manual (`git tag vX.Y.0 && git push origin vX.Y.0`).
- If no matching tag exists, fall back to `v0.1.0`.
- Skip entirely (no tag, no release) if the triggering commit message contains the literal string `[skip release]`.
- The workflow needs `contents: write` permission (the only workflow in this repo that does — `ci.yml`, `codeql.yml`, `govulncheck.yml`, `secret-scan.yml` stay read-only).
- Use a `concurrency` group `release-main` with `cancel-in-progress: false` so overlapping runs queue instead of racing.
- Follow this repo's existing action versions for consistency: `actions/checkout@v7`.
- Reference spec: `docs/superpowers/specs/2026-08-12-auto-tag-release-design.md`.

---

### Task 1: Version-resolution script

**Files:**
- Create: `scripts/next-version.sh`
- Test: `tests/scripts/test_next_version.sh`

**Interfaces:**
- Produces: `scripts/next-version.sh` — a shell script with no arguments. Run as `bash scripts/next-version.sh` from inside any git working directory. Prints exactly one line to stdout: the next tag name (e.g. `v1.5.3`). Reads tags from the git repo in the current working directory (`git tag --list`). Task 2 invokes it exactly this way.

- [ ] **Step 1: Write the failing test**

Create `tests/scripts/test_next_version.sh`:

```bash
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash tests/scripts/test_next_version.sh`
Expected: FAIL — `scripts/next-version.sh: No such file or directory` (script doesn't exist yet).

- [ ] **Step 3: Write the implementation**

Create `scripts/next-version.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

latest=$(git tag --list | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1)

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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash tests/scripts/test_next_version.sh`
Expected: `All next-version tests passed.`

- [ ] **Step 5: Verify against this repo's real tag history**

Run: `bash scripts/next-version.sh` (from the repo root, `C:\Users\DanielGottschalck\Dev\Znow\breeze`)
Expected: `v1.5.3` (current highest matching tag is `v1.5.2`).

- [ ] **Step 6: Commit**

```bash
git add scripts/next-version.sh tests/scripts/test_next_version.sh
git commit -m "Add next-version.sh script for computing the next patch tag"
```

---

### Task 2: Release workflow

**Files:**
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `scripts/next-version.sh` (Task 1) — invoked as `bash scripts/next-version.sh`, prints the next tag name to stdout.

- [ ] **Step 1: Write the workflow file**

Create `.github/workflows/release.yml`:

```yaml
name: Release

on:
  push:
    branches: [main]
  workflow_dispatch:
    inputs:
      dry_run:
        description: "Compute the next version without tagging or creating a release"
        type: boolean
        default: true

concurrency:
  group: release-main
  cancel-in-progress: false

permissions:
  contents: write

jobs:
  tag-and-release:
    if: ${{ !contains(github.event.head_commit.message || '', '[skip release]') }}
    runs-on: ubuntu-latest
    timeout-minutes: 10

    steps:
      - name: Checkout code
        uses: actions/checkout@v7
        with:
          fetch-depth: 0

      - name: Compute next version
        id: version
        run: |
          NEXT_VERSION=$(bash scripts/next-version.sh)
          echo "Computed next version: $NEXT_VERSION"
          echo "next=$NEXT_VERSION" >> "$GITHUB_OUTPUT"

      - name: Configure git identity
        if: ${{ !(github.event_name == 'workflow_dispatch' && inputs.dry_run) }}
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"

      - name: Tag and push
        if: ${{ !(github.event_name == 'workflow_dispatch' && inputs.dry_run) }}
        run: |
          git tag "${{ steps.version.outputs.next }}"
          git push origin "${{ steps.version.outputs.next }}"

      - name: Create GitHub release
        if: ${{ !(github.event_name == 'workflow_dispatch' && inputs.dry_run) }}
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: gh release create "${{ steps.version.outputs.next }}" --generate-notes
```

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "Add release workflow to auto-tag and release on merge to main"
```

- [ ] **Step 3: Push a test branch and trigger a dry run**

```bash
git checkout -b feature/auto-tag-release-workflow
git push -u origin feature/auto-tag-release-workflow
gh workflow run release.yml --ref feature/auto-tag-release-workflow
```

Expected: command exits without error (confirms the YAML is well-formed and GitHub accepted the trigger — a malformed workflow file causes `gh workflow run` to fail here).

- [ ] **Step 4: Watch the run and verify its output**

```bash
gh run watch --exit-status $(gh run list --workflow=release.yml --branch=feature/auto-tag-release-workflow --limit=1 --json databaseId --jq '.[0].databaseId')
```

Expected: run succeeds; the "Compute next version" step log shows `Computed next version: v1.5.3` (matching Task 1 Step 5's local result); the "Configure git identity", "Tag and push", and "Create GitHub release" steps show as skipped (because `dry_run` defaulted to `true`).

- [ ] **Step 5: Verify no tag or release was created**

```bash
git ls-remote --tags origin | grep v1.5.3 || echo "no tag created, as expected"
gh release list --limit 5
```

Expected: `no tag created, as expected` is printed, and `v1.5.3` does not appear in the release list.

---

### Task 3: Open PR for review

**Files:** none (repo state only)

- [ ] **Step 1: Push the branch (if not already pushed in Task 2) and open a PR**

```bash
git push -u origin feature/auto-tag-release-workflow
gh pr create --title "Add auto-tag and release workflow" --body "$(cat <<'EOF'
## Summary
- Adds .github/workflows/release.yml, which tags and releases a patch-bumped
  version on every merge to main (skippable via `[skip release]` in the
  commit message).
- Adds scripts/next-version.sh (+ tests) computing the next version from
  existing vX.Y.Z tags.

Design: docs/superpowers/specs/2026-08-12-auto-tag-release-design.md
Plan: docs/superpowers/plans/2026-08-12-auto-tag-release-workflow.md

## Test plan
- [x] `bash tests/scripts/test_next_version.sh` passes locally
- [x] Dry-run trigger (`gh workflow run release.yml --ref feature/auto-tag-release-workflow`)
      confirms the workflow computes v1.5.3 without creating a tag or release
- [ ] After merge, confirm the workflow runs for real and creates tag `v1.5.3`
      and a matching GitHub Release
EOF
)"
```

Expected: PR created; command prints the PR URL.

- [ ] **Step 2: Hand off for merge**

Do not merge this PR automatically — merging to `main` is the action that makes the workflow tag and release for real (creating `v1.5.3` and a public GitHub Release). Report the PR URL to the user and let them merge when ready.

## Self-Review Notes

- Spec coverage: trigger (Task 2 Step 1), concurrency group (Task 2 Step 1), skip-on-`[skip release]` (Task 2 Step 1 job `if`), version resolution + malformed-tag exclusion + fallback (Task 1), tag+push+release (Task 2 Step 1), `contents: write` (Task 2 Step 1), manual-bump compatibility (unchanged — script always reads current tags at run time, verified by Task 1's design). All covered.
- No placeholders: every step has literal code/commands, no "TBD" or "add appropriate handling".
- Type/name consistency: `scripts/next-version.sh` is invoked identically in Task 1 Step 5 and Task 2 Step 1 (`bash scripts/next-version.sh`); output variable name `next` in `steps.version.outputs.next` is defined once (Task 2 Step 1) and used consistently in the same task.
