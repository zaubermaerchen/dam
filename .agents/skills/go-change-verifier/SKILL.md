---
name: go-change-verifier
description: Verify changes in this Go repository before commit, pull request, review, or merge. Use when asked to validate changes, run a preflight, check PR readiness, or report verification evidence; do not use to implement or repair changes unless separately requested.
---

# Go Change Verifier

Verify the requested change without modifying source files or expanding the user's authorization. Read the repository's `AGENTS.md` completely and treat it as the source of truth for required checks.

## Establish the scope

- Determine whether the target is the working tree, staged changes, a commit range, or a pull request. Infer the smallest reasonable scope when the user does not specify one.
- Inspect the selected target's actual diff: the working tree and index for local changes, the requested base and head for a commit range, or the pull request's base and head for a PR.
- Also inspect local status and staged and unstaged diffs when they can reveal changes outside the selected target. Preserve unrelated or pre-existing changes.
- Identify Go files, tests, build tags, module files, CI configuration, concurrency, and OS-specific behavior affected by the change.

## Select and run checks

Run checks that provide evidence for the actual change. Do not claim full verification when a required check cannot run.

For every change:

- Check applicable diffs for whitespace errors, including staged changes when present.
- Run repository-specific validation required by `AGENTS.md` or CI.

For Go source or test changes:

- Check formatting without rewriting files.
- Run `go test ./...` and `go vet ./...`.
- Run `go test -race ./...` when concurrency, signals, process lifecycle, shared state, or pre-merge full verification is in scope.

For platform-specific changes:

- Validate every affected build-tag target.
- Prefer native tests when the environment provides the target OS.
- Otherwise cross-compile affected packages and test binaries into a writable temporary directory. State clearly that cross-compiled tests were compiled but not executed.

For pull-request readiness:

- Inspect the current PR checks when a GitHub integration is available.
- Wait for required checks when merge readiness is the requested outcome; report pending checks otherwise.
- Treat required CI failures as blockers and include the failing job URL when available.

Documentation-only changes do not require new tests. Run existing tests only when repository policy, CI impact, or the requested confidence level makes them relevant.

## Execution constraints

- Keep verification read-only: do not format in place, edit files, commit, push, reply to reviews, resolve threads, rerun CI, or change PR state without separate authorization.
- Continue independent checks after one fails when doing so is safe, so the report shows the complete failure set.
- If the default Go caches are outside the writable sandbox, redirect `GOCACHE` and `GOMODCACHE` to an allowed temporary location and rerun the affected check.
- Write generated binaries and other temporary artifacts outside the repository.

## Report

Lead with `PASS`, `FAIL`, or `PARTIAL`, then provide:

- the verified scope;
- each check and its result;
- concise failure evidence and blocker status;
- skipped or unavailable checks with reasons;
- any remaining manual or native-platform verification.

Do not describe a change as merge-ready unless all repository-required local checks and required PR checks have passed.
