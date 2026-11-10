# Phase 5: Publish

Acceptance bar: spec §15 Phase 5 row. Done when "Installed on a project with zero repo
footprint": `TestPhase5ZeroRepoFootprint` plus the manual dry run in `tasks/progress.md`.
Earlier phase plans are in the git history of this file (Phase 2 plan: `6bc02ca`).

Conventions kept: every state change in one `BEGIN IMMEDIATE` write; lock order task lock ->
`git.lock`; argv-only git/tmux; human-only actions through `requireHuman`; every file checked
before any is written; byte-exact edits of user files. Outward actions (push, tag, release,
tap repo, secrets) are Kenneth's, not this plan's.

## Commands

- [x] 1. `version`: `-ldflags -X main.version/commit/date`, else `debug.ReadBuildInfo`, else `dev`
  (`internal/buildinfo`, `TestVersionLdflags`).
- [x] 2. `uninstall`: hook entries matched by basename `shoalctl` + first arg `hook` under any event,
  the managed block (same column-0, fence-aware scan as init), unedited `shoal-*` agents
  (user-level only when no other project is registered). Byte-exact round trips; refuse before
  writing (exit 3).
- [x] 3. `export <id> [--out]`: every table but `memory_fts`, in one transaction, plus reports, as JSON.
- [x] 4. `purge <id>`: human only, type the id; refused with worktrees left (exit 5) or locks held
  (exit 8); rename aside, then delete.
- [x] 5. `gc`: closed tasks under their lock: close window, `git worktree remove` (kept, exit 5),
  `DropWorkingRows`; flag records whose files are all gone from `[merge].base`.

## Release

- [x] 6. `.goreleaser.yaml`: CGO_ENABLED=0, darwin/linux/windows x amd64/arm64, `-trimpath`, commit
  timestamps (reproducible), archives = binary + LICENSE + README.md, `checksums.txt`, formula
  for `kennethsolomon/homebrew-tap` (written by GoReleaser, pushed by release.yml for stable tags
  with a non-empty `HOMEBREW_TAP_GITHUB_TOKEN`).
- [x] 7. `internal/releaseguard`: fails on `.db` (and companions), `reports/`, stray files, a missing
  binary, unlisted or wrong checksums. Planted-file tests.
- [x] 8. `.github/workflows/release.yml`, on `v*` tags only: FTS5 spike on six native runners, full
  suite on Linux/macOS (Windows non-blocking), one build to a draft release, guard, draft assets
  checked against the guarded checksums, publish the draft, formula push. `ci.yml` untouched.

## Acceptance and docs

- [x] 9. `TestPhase5ZeroRepoFootprint`, `TestPhase5GCAfterMerge`, `TestPhase5GCSkipsOpenTasks`.
- [x] 10. Snapshot release and manual dry run recorded in `tasks/progress.md`.
- [x] 11. README install (go install, brew, archive + checksum; still pre-alpha), spec §2, §4.4,
  §4.6, §5.1, §7.1, §7.4, §13, §15, §16 rows 77-82.
- [x] 12. Gates from a fresh clone: gofmt, vet x3 GOOS, staticcheck x3 GOOS, `go test -race`,
  `go test -c` linux/windows per package, build-tag ignored files, `git ls-files -oi --exclude-standard`.

## Verification

```sh
gofmt -l .
for os in darwin linux windows; do GOOS=$os go vet ./... && GOOS=$os ~/go/bin/staticcheck ./...; done
caffeinate -i go test -race -count=1 ./...
for os in linux windows; do for p in $(go list ./...); do GOOS=$os go test -c -o /dev/null $p; done; done
goreleaser check; goreleaser release --snapshot --clean
SHOAL_DIST=$PWD/dist go test -count=1 -run TestReleaseArtifacts ./internal/releaseguard
```

## Risks

See `tasks/progress.md`, Phase 5 "Open risks": untested runner labels, deprecated `brews`,
the draft-release flow not run on GitHub yet, branches and exclude lines kept.
