# Workspace sandbox lookup implementation plan

**Goal:** Recover an existing usable sandbox ID after the App loses its workspace mapping, without taking over or deleting its owner.

**Architecture:** Read the exact workspace owner using configured storage identity, then validate its authoritative lifecycle snapshot and runtime. Use read-only inventory for historical uncoordinated sessions; expose the result through HTTP and the Go SDK.

**Tech Stack:** Go, Gin, existing atomic state store and active sandbox repository.

Spec: `docs/superpowers/specs/2026-10-06-workspace-sandbox-lookup-design.md`.

## Task 1: Manager lookup

- [x] Add `internal/sandbox/workspace_lookup_test.go`: cross-replica lookup, legacy sessions without migration, owner/runtime mismatch, lifecycle conflicts, ambiguous candidates and read failures; assert no destruction or owner mutation.
- [x] Run `go test ./internal/sandbox -run 'TestWorkspaceLookup' -count=1` and confirm the missing method causes failure.
- [x] Add `internal/sandbox/workspace_lookup.go` with `GetByWorkspace(context.Context, string) (Sandbox, error)` and stable lookup errors. Read owner by canonical storage key, match persisted owner and immutable runtime identity, verify FUSE health, and re-read owner/lifecycle before returning.
- [x] Add read-only current/legacy session inventory to `internal/sandbox/session.go`; never call migration, renewal, save, owner recovery or destruction.
- [x] Re-run manager tests.

## Task 2: HTTP and SDK

- [x] Add router and SDK tests for static route precedence, authentication, success metadata, URL encoding and 400/404/409/503 codes; run to observe failure.
- [x] Add `GET /api/v1/sandboxes/by-workspace?workspace_path=...` and handler; add `workspace_path` to server and SDK `SandboxResponse`.
- [x] Add `Client.GetSandboxByWorkspace(ctx, workspacePath)` using `url.Values` and existing HTTP error handling.
- [x] Run `go test ./internal/api/... ./pkg/types` and `(cd sdk/go && go test ./...)`.

## Task 3: Documentation and verification

- [x] Document the HTTP endpoint and App recovery/retry flow in `README.md` and `sdk/go/README.md`, including service API key versus tenant authorization scope.
- [x] Format Go changes, inspect diff and check spec coverage.
- [x] Run affected packages under `go test -race`, SDK tests under `-race`, and `go vet` on affected packages.
- [x] Review the whole change and resolve correctness findings; leave changes reviewable in the current feature checkout, preserving the unrelated user edit.

## Execution notes

The user approved implementation in this chat. Execute inline without another approval gate. The current checkout is already on `feat/workspace-fuse-mount`; no branch switch is needed.

## Verification results

- Manager/API/Docker affected package suites passed under `go test -race`.
- SDK and smoke command package suites passed under `go test -race ./...`.
- `golangci-lint run` on affected server packages: 0 issues.
- `go vet` passed for affected server packages and all SDK packages.
- `git diff --check` passed.
- Review findings fixed with regression tests: legacy backend proof, Docker not-found normalization, owner/lease publication during absence scan; unrelated closed gates no longer block absence.
- Historical ownerless object-store sync records return 409 because backend scope cannot be proved. Old Docker local sessions use actual writable bind mounts to prove scope.
