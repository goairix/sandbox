# Optional AppArmor Loader Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a bounded, fail-closed node loader managing one content-addressed enforce profile without unloading or replacing existing policies.

**Architecture:** A standard-library Go core validates immutable policy bytes before any parser invocation, checks module and exact securityfs profile state, and writes short-lived readiness evidence. A signal-aware CLI invokes the trusted parser with fixed flags and stdin, and an exec readiness probe checks timestamp plus process identity.

**Tech Stack:** Go 1.25, AppArmor parser, Debian 13 multi-platform container.

---

Scope: only `internal/apparmorloader/*`, `cmd/apparmor-loader/*`, `docker/images/apparmor-loader/*` and this plan. The parent implements Chart/runtime integration and commits the reviewed batch. This delegated task does not commit, build images or mutate the cluster; the user builds release images separately.

### Task 1: Policy identity

**Files:** Create `internal/apparmorloader/policy.go`; test `internal/apparmorloader/policy_test.go`.

- [x] Write table-driven identity and rejection tests using this canonical fixture:

```go
const policyTemplate = "profile __SANDBOX_PROFILE_NAME__ flags=(attach_disconnected,mediate_deleted) {\n  /dev/fuse rw,\n}\n"
sum := sha256.Sum256([]byte(policyTemplate))
digest := hex.EncodeToString(sum[:])
name := "sandbox-fuse-" + digest
rendered := strings.Replace(policyTemplate, "__SANDBOX_PROFILE_NAME__", name, 1)
if err := ValidatePolicy([]byte(rendered), name, digest); err != nil { t.Fatal(err) }
```

- [x] Run `go test ./internal/apparmorloader -run TestValidatePolicy -v`, confirm missing implementation RED.
- [x] Implement canonicalization as CRLF-to-LF, surrounding whitespace trim and one final LF; validate exactly one named declaration, matching full lowercase SHA256, balanced sole profile body, no includes, complain flags, hats or child profiles.
- [x] Run the same command and confirm GREEN.

### Task 2: Verification and retry lifecycle

**Files:** Create `internal/apparmorloader/loader.go`, `internal/apparmorloader/readiness.go`; test `internal/apparmorloader/loader_test.go`.

- [x] Add temp-file tests using module `Y`, profiles `<name> (enforce)\n`, rendered policy and fake parser callback. Verify disabled module, read failures, complain, duplicate names, immutable policy mismatch and parser failure return errors and remove readiness. Missing profiles invoke parser exactly once then re-read kernel; enforce existing profiles do not invoke parser. Test failed checks recover and cancelled `Run` removes readiness without invoking unload.
- [x] Run `go test ./internal/apparmorloader -run 'Test(Check|Run|Readiness)' -v`, confirm RED.
- [x] Implement `Config`, `New`, `Loader.Check(context.Context) error`, `Loader.Run(context.Context) error`, bounded parser context and JSON readiness containing PID, process-start identity and verification timestamp. Probe requires live same-identity process and recent timestamp; remove stale file before each verification and on exit.
- [x] Run `go test -race ./internal/apparmorloader`, confirm GREEN.

### Task 3: Trusted process and container boundary

**Files:** Create `internal/apparmorloader/parser.go`, `internal/apparmorloader/parser_test.go`, `cmd/apparmor-loader/main.go`, `cmd/apparmor-loader/main_test.go`, `docker/images/apparmor-loader/Dockerfile`, `docker/images/apparmor-loader/README.md`.

- [x] Add parser helper-process test expecting fixed `--add --skip-cache` arguments and exact stdin; timeout and output-redaction tests. Add CLI invalid configuration and readiness argument tests; run `go test ./internal/apparmorloader ./cmd/apparmor-loader` to confirm RED.
- [x] Implement `ExecParser` with direct `exec.CommandContext` (no shell), fixed trusted binary path and flags, bounded captured diagnostic output not surfaced as tenant/profile content. Wire CLI flags and SIGINT/SIGTERM cancellation, and readiness subcommand.
- [x] Package static loader from Go 1.25 into Debian 13 slim with installed `apparmor`; document amd64/arm64 build commands and exact restricted mounts. No image build or live kernel tests in this task.
- [x] Run `go test -race ./internal/apparmorloader ./cmd/apparmor-loader` and `go vet ./internal/apparmorloader ./cmd/apparmor-loader`. Report kernel enforcement remains unverified on macOS.

## Execution evidence

All three tasks were implemented in the authorized shared branch. Initial RED runs failed on the expected missing production APIs. Additional behavior-level RED runs rejected stopped-process readiness and exposed compact nested-profile syntax; their fixes were followed by GREEN runs. Latest verification: `go test -race ./internal/apparmorloader ./cmd/apparmor-loader` and `go vet ./internal/apparmorloader ./cmd/apparmor-loader` both exited 0. `GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/apparmor-loader` and the corresponding `arm64` command both exited 0. No image was built, no commit/push occurred, and no live cluster/kernel operations were performed. Actual Linux policy syntax and kernel/mounter enforcement remain deployment acceptance work; the parent owns the real policy and Helm/runtime integration.

### Code-review hardening: quoted comments and inherited execution

Two P2 findings were reproduced independently with content-addressed matching-digest policy templates before fixes. `TestValidatePolicyQuotedHashCannotHideRules` was RED because quoted `#` and escaped-quote/`#` pathnames hid a following nested profile, because unterminated quotes were accepted, and because quoted keyword paths were incorrectly rejected. The scanner now masks quoted pathname contents and real comments with escape-aware quote boundaries, preserves following structural tokens, and rejects incomplete quotes/escapes. Normal quoted hashes, escaped quotes/backslashes and comments containing unmatched quotes are GREEN.

`TestValidatePolicyInheritanceOnlyExecution` was separately RED for leading/trailing `ux`, `Ux`, `px`, `Px`, `cx`, `Cx`, `pix`, `Pix`, `cix`, `Cix`, `pux`, `PUx`, `cux`, `CUx` and bare `x` permissions. The validator now permits execution only through `ix` plus ordinary file permissions; it rejects all other execution modes, including profile transitions with inheritance fallback. Bare `x` is conservatively rejected as well; this bounded template validator does not accept deny-execute syntax. Quoted path/comment strings mentioning execution modes, mount `exec` options and `network unix` do not cause false positives. The execution grammar and mode semantics were checked against the primary [Debian AppArmor policy manpage](https://manpages.debian.org/trixie/apparmor/apparmor.d.5.en.html).

Post-fix verification: `go test -race -count=1 ./internal/apparmorloader ./cmd/apparmor-loader`, `go vet ./internal/apparmorloader ./cmd/apparmor-loader`, and both Linux `amd64`/`arm64` builds to `/dev/null` passed. Only core-owned files and this plan were changed; no commits, images or live kernel tests were performed.

### Code-review hardening: external ABI metadata

The primary [Debian policy manpage](https://manpages.debian.org/trixie/apparmor/apparmor.d.5.en.html) defines ABI directives using absolute or magic paths to external feature metadata. A matching template hash therefore did not make such a policy self-contained. `TestValidatePolicyExternalABIWithMatchingDigest` was first RED: preamble/body ABI magic and quoted absolute paths, an inline directive after a file rule, adjacent keyword/path syntax and tab-separated syntax were accepted. The validator now rejects ABI directive tokens in quote/comment-masked code before block validation, including before, inside and after the profile declaration. Comments, ordinary `/tmp/abi` paths and quoted/escaped-quote paths containing ABI text remain accepted. The current Chart candidate contains no ABI directives; this is core contract hardening, not a current Chart policy defect.

GREEN verification: `go test -race -count=1 ./internal/apparmorloader ./cmd/apparmor-loader`, `go vet ./internal/apparmorloader ./cmd/apparmor-loader`, and Linux `amd64`/`arm64` builds to `/dev/null` all exited 0. Changes are limited to owned policy/test, loader README and this plan. No image build, host policy load, cluster mutation or commit occurred.
