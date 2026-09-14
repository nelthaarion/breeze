# Auto-tag & release workflow — design

## Purpose

`Znow/breeze` is a fork of `nelthaarion/breeze` maintained so that `techcoach-breeze` can pin
its Go module dependency to a stable, human-readable tag (via a `go.mod` `replace` directive)
instead of a pseudo-version commit pin. Today, cutting a new tag is a manual two-command
process (`git tag vX.Y.Z && git push origin vX.Y.Z`). This design automates that so every merge
to `main` produces a new tag and GitHub Release without manual steps, while still allowing
manual minor/major bumps when warranted.

## Trigger

```yaml
on:
  push:
    branches: [main]
```

PR merges land as pushes to `main`, so this covers the normal merge flow. Direct pushes to
`main` (rare in practice) also trigger it, which is the desired behavior — any change landing
on `main` should get a version.

## Concurrency

A single shared group, `cancel-in-progress: false`:

```yaml
concurrency:
  group: release-main
  cancel-in-progress: false
```

If two merges land close together, the second run must **wait** for the first to finish (not
be cancelled), so it computes "current highest tag" only after the first run's tag has already
been pushed. This prevents both runs from independently computing the same next version.

## Skip mechanism

If the triggering commit's message contains the literal string `[skip release]`, the job exits
immediately after checkout — no tag, no release. This is the only opt-out; no path-based (e.g.
docs-only) guard is included, per explicit decision during design review.

## Version resolution

1. List tags matching the regex `^v[0-9]+\.[0-9]+\.[0-9]+$` only. Malformed entries
   (`v.1.4.1`, bare `1.0.1`, `v1.0`) must be excluded from consideration.
2. Sort matching tags by semver and take the highest.
3. Bump the **patch** segment only: `v0.1.0` → `v0.1.1`. Minor/major bumps are never automatic.
4. If no tag matches the regex at all, fall back to `v0.1.0`.

**Which tags count.** Only tags on `Znow/breeze` (`origin`) — the workflow runs against the
fork, and `actions/checkout` fetches only that remote's tags. The `v1.x` history
(`v1.5.2`, `v1.3.x`) and the malformed entries above all live on `upstream`
(`nelthaarion/breeze`); none of them were ever on the fork. A local clone with `upstream`
fetched shows the union of both, which is misleading — an earlier draft of this spec read
local tag state and wrongly asserted the fork's highest tag was `v1.5.2`.

**Version line.** As of 2026-08-13 the fork had no tags, so the first workflow run took the
fallback and cut `v0.1.0`; subsequent merges produce `v0.1.1`, `v0.1.2`, and so on. The fork
therefore versions **independently of upstream** and will not track upstream's `v1.5.x`
numbering. This was a deliberate choice — the alternative (seeding the fork with upstream's
`v1.5.2` so auto-bumps continue from `v1.5.3`) was considered and declined.

## Tagging & release

1. `git tag v<new>` on the commit that triggered the workflow (i.e., the new tip of `main`).
2. `git push origin v<new>`.
3. `gh release create v<new> --generate-notes` — GitHub auto-generates release notes from
   merged PRs and commits since the previous tag.

## Permissions

This is the only workflow in the repo that needs `contents: write` (to push tags and create
releases). Existing workflows (`ci.yml`, `codeql.yml`, `govulncheck.yml`, `secret-scan.yml`)
remain read-only and are unaffected.

## Interaction with manual tagging

Manual minor/major bumps continue to work exactly as before: `git tag v1.6.0 && git push origin
v1.6.0`. Because version resolution always reads the *current* highest matching tag at run time,
the next automatic run picks up from wherever a manual tag left off (e.g. after a manual
`v1.6.0`, the next auto-run produces `v1.6.1`).

Consumers (e.g. `techcoach-breeze`) continue to point at a specific tag via:

```
go mod edit -replace github.com/nelthaarion/breeze=github.com/Znow/breeze@vX.Y.Z
go mod tidy
```

Bumping to a newer auto-cut tag is a manual, deliberate action in the consumer repo — this
workflow does not push updates downstream.

## Out of scope

- Conventional-commit or label-driven bump-type detection (deferred; always-patch was chosen
  for simplicity and to match current low-ceremony usage of this fork).
- Automatically notifying or updating `techcoach-breeze` when a new tag is cut.
- Docs-only path-based skip guard (explicitly declined; `[skip release]` is the sole override).
