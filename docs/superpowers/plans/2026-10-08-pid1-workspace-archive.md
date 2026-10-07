# PID1 workspace archive Implementation Plan

> REQUIRED: subagent-driven-development; one fresh implementer and one independent task/unit reviewer. Execute this scoped approved parent requirement without a new permission checkpoint.

**Goal:** real confined readonly archive creation after observed user-process drain, ready for subsequent authenticated Supervisor integration.

**Spec:** docs/superpowers/specs/2026-10-08-pid1-workspace-archive-design.md

## Global Constraints

- Existing codex/etcd-state-management checkout; no duplicate worktree/task, no unrelated Sentinel file reads/edits/staging.
- Source/tests/spec/plan/report writes only apply_patch, including formatting patches; no cat/heredoc or gofmt -w source edits.
- Physical source only; no metadata signing, effects ledger, provider upload, release, production wiring or durable-barrier claims.
- No old accepted fixture scenario replay. Native tests select only new archive tests; image pinned and pull never; exact owned cleanup.
- Preserve complete raw logs/commands/exits/environment/hash, diagnostics and SKIPs in this plan's OWN. No ignored OWN force-add.
- Small coherent commits, exact stage paths, empty index before/after, OWN/git-commit.lock covering stage through commit.

### Task 1: Fixed PID1 archive source and focused native validation

**Files:** Create internal/runtime/launcher/workspace_archive_types.go, workspace_archive_linux.go, workspace_archive_unsupported.go, workspace_archive_test.go, workspace_archive_native_test.go and scripts/test-workspace-archive.sh. Avoid changes to existing bootstrap/gate/monitor tests and infrastructure. Add another focused test file only if required and report exact ownership.

**Consumes:** original KernelBoundary/self/role/PID/seal, original UserNamespaceQuiescenceObservation/RevalidateCurrent; original requireNativeBoundary helper. **Produces:** OpenWorkspaceArchive, original single-use WriteArchive/Close and diagnostic WorkspaceArchiveSummary defined by Spec. Linux openat2 native confinement and bounded synchronous tar stream.

- [ ] Step 1 RED: meaningful positive native archive assertion fails with compilable all-reject temporary implementation; retain raw failure and stub snapshot. Add failure cases against actual namespace/file descriptors, without claiming a fake filesystem proves confinement.
- [ ] Step 2 implement the complete Spec, including all error cleanup, no-follow/nonblocking kind validation, bounded native traversal, single-consumption lifecycle and zero summary on failures.
- [ ] Step 3 GREEN: focused new native archive fixture, affected launcher host race once, vet, Linux arm64 CGO0 compile. Capture exact Docker native terminal state, all diagnostics and cleanup inventories. Classify skipped old native tests honestly; no full kernel gate replay.
- [ ] Step 4 self-review and source commit under OWN lock; author evidence report; independent sole task/unit review and Root adjudication of every limitation; separate tracked acceptance commit.

## Review Focus

Confused deputy/root escape; copy-before-lock rejection; symlink/FIFO races; cross-mount/hardlink semantics; FD and memory bounds; root replacement; writer failure/short writes; byte limits including final TAR blocks; non-reconstructible original handles; cancellation is not physical completion; original observation does not imply closed users gate.
