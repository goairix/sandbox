# Protected runtime target journal verification

This intermediate unit is verified and independently reviewed; complete production migration remains pending. Exact Go source4dd09c8 and final script34b9a03; full final review7642fc1 plus scoped fix review close all current findings. No push, merge or deployment. The full saved reports below preserve every actual encountered implementation/test/setup error, source association, counts, skip distinctions, capacity and cost boundary. Scratch paths in copied reports are historical provenance; this plan workspace is removed after archival. Complete reports and command/exit/metadata evidence are retained here, rather than claiming historical scratch logs still exist.


Source contracts: passive closed/unknown only, no execution authority or production state switch. Existing Sentinel edit excluded. Tasks1/2 passed independent gates, Task3 and final integrated gates pending.

## Complete saved task-1-report.md

# Task 1 report: strict passive journal metadata and wire

Status: DONE. Independent controller review remains required; this self-review is not that gate.

## Source and scope

- Workspace `/Users/dysodeng/project/go/cloud/sandbox`; branch `codex/etcd-state-management`.
- Exact starting BASE `9fd10b4353b39e36312f899501f64e146c6241f3` verified before edits.
- Slice 1 commit `3e6e8b7` — `feat(runtime): define and validate passive journal metadata` (3 files, 283 inserted lines).
- Slice 2 commit `4b09c66` — `feat(runtime): add strict canonical journal wire codec` (2 files, 524 inserted lines).
- Only source files created: `internal/runtime/controltarget/journal_types.go`, `journal_validation.go`, `journal_codec.go`, `journal_codec_test.go`.
- Unrelated Sentinel tracked modification was never read, edited or staged. Final status also showed the controller-owned journal design spec modified; it was not read or staged by this worker.
- Read own brief and FULL own context, implementer prompt, TDD/verification skill instructions and TDD test reference. Read only exact existing controlprotocol model/codec/shape-validator files for compatibility. No full plan/history/sibling workspace reads, credentials, `.env`, ignored user content, Docker or fixture activity.

## Produced interfaces and semantics for Task 2/3

Package `controltarget`:

```go
func (i JournalIdentity) Validate() error
func (m GateManifest) Validate() error
func (r ExecJournalRecord) Validate() error
func encodeGateManifest(m GateManifest) ([]byte, error)
func decodeGateManifest(w []byte, m *GateManifest) error
func encodeExecJournalRecord(r ExecJournalRecord) ([]byte, error)
func decodeExecJournalRecord(w []byte, r *ExecJournalRecord) error
```

All shape/wire violations match `ErrInvalidRecord` with `errors.Is`; decoding failures leave the entire destination unchanged, including typed decode/semantic validation failures. Nil destinations reject. Underlying parse/typed/encode causes are retained where available. No public shared/global protocol validator was introduced.

Persisted exact fields:

- `JournalIdentity`: `Namespace string` (`namespace`), `AuthorityID string` (`authority_id`), `Target string` (`target`), `RestoreEpoch string` (`restore_epoch`), `SandboxID string` (`sandbox_id`), `WorkspaceHash string` (`workspace_hash`), `Generation int64` (`generation`), `Runtime controlprotocol.RuntimeReference` (`runtime`). Runtime's existing required fields/tags are `ID string/id`, `UID string/uid`, `BootID string/boot_id`.
- `GateManifest`: `Version uint32/version`, `Identity JournalIdentity/identity`, `DataGateEpoch int64/data_gate_epoch`, `GateState string/gate_state`.
- `ExecJournalRecord`: `Version uint32/version`, `State string/state`, `Context controlprotocol.ExecStartContext/context`, `DescriptorDigest string/descriptor_digest`, `TicketDigest string/ticket_digest`, `NotBefore time.Time/not_before`, `NotAfter time.Time/not_after`.
- Context uses existing controlprotocol exact types/tags: strings `Namespace/namespace`, `AuthorityID/authority_id`, `Target/target`, `RestoreEpoch/restore_epoch`, `IssuerCertificateID/issuer_certificate_id`, `IssuerCertificateDigest/issuer_certificate_digest`, `CommandID/command_id`, `OperationID/operation_id`, `RequestID/request_id`, `OperationDigest/operation_digest`, `SandboxID/sandbox_id`, `WorkspaceHash/workspace_hash`; int64 `Generation/generation`, `DataGateEpoch/data_gate_epoch`, `ControlRevision/control_revision`, `AdmissionRevision/admission_revision`, `LeaseID/lease_id`; `Runtime RuntimeReference/runtime`; `ExpiresAt time.Time/expires_at`.
- Nonwire `JournalOptions`: `Directory string`, `Identity JournalIdentity`, `DataGateEpoch int64`, `ManagementUID uint32`, `MaxBytes int64`.
- Nonwire `JournalStatus`: `Gate GateManifest`, `LogicalBytes int64`, `Records uint64`, `TemporaryFiles uint64`, booleans `Warning`, `NewRecordsStopped`, `Poisoned`, `AccountingKnown`, `Closed`.
- Sentinel errors: `ErrInvalidConfiguration`, `ErrInvalidRecord`, `ErrIdentityMismatch`, `ErrConflict`, `ErrBusy`, `ErrCapacity`, `ErrJournalUnavailable`, `ErrClosed`.
- Private wire bounds: `maxJournalWireBytes=8192`, `maxJournalJSONDepth=8`. Other private codec/shape helpers remain implementation details.

Identity shape preserves existing controlprotocol namespace rules, including >=3 legal nonempty segments, <=512 bytes, trailing/leading slash and <=128-byte final two segments. Opaque authority/target/runtime strings preserve exact UTF-8 and permit characters such as `<&>`; they reject control characters, empty and >128-byte values. IDs use the existing ASCII segment constraints; hashes are lowercase 64hex and UUIDs canonical lowercase nonnil. All positive generation/gate/revision/Lease constraints apply. Time is nonzero, exactly `time.UTC`, JSON-encodable year 0..9999; wire time strings must equal parsed RFC3339Nano rendering, requiring canonical `Z` and rejecting offsets, comma fractions and redundant fraction zeros. NB < NA, span <=30s, and NA <= business ExpiresAt.

Controller confirmed and implementation follows: `GateManifest.Validate` and decoder accept historical open/closed; producer `encodeGateManifest` accepts only closed. Historical open input is generated with typed `json.Marshal` in tests, never through the closed encoder. `State` can only be unknown, and neither field creates a capability. `JournalStatus.NewRecordsStopped` is intended for new passive unknown record capacity/poison/closed-handle restrictions, not physical admission authority; Task 1 defines the scalar field only.

Canonical output uses struct field order, `SetEscapeHTML(false)`, no trailing newline, and exact opaque characters/time nanoseconds. All data are copied scalar structs; no slices/full tickets or argv/env/stdin/stdout/file payloads persist. Task 3 must compare COMPLETE output of `encodeExecJournalRecord`, not only digests or command IDs, and additionally bind the context to current identity/gate epoch. Such current-journal binding has no receiver/state to compare against in Task 1 and remains Task 2/3 responsibility.

## TDD and actual verification

All commands run from the own workspace. No build/setup failures were used as RED.

1. A compilable metadata stub returned nil from Validate; tests were written before validation behavior. Ran `go test ./internal/runtime/controltarget -run '^TestJournal(Codec|Identity)' -count=1 -v` with output captured in `task-1-red-validation.log`. It genuinely failed `invalid identity accepted`/`invalid manifest accepted`/`invalid record accepted`: 61 FAIL nodes, 0 PASS, 0 SKIP. The first shell invocation then tailed its output without preserving test exit, so the outer shell returned 0; corrected immediately by rerunning the exact test command directly, whose actual exit was 1 and expected failures were inspected. This direct rerun output was tool-truncated, not a hidden setup failure; complete original RED is in the log.
2. Implemented shape/interval validation. Exact focused `go test ... -v` exit 0: 61 PASS nodes (3 top-level +58 subtests), no failures or skips. Tool display truncated this verbose run; it reported PASS and exit 0. No validation GREEN log was captured for that first run.
3. Before slice 1 commit: `go test -race ./internal/runtime/controltarget -run '^TestJournal(Codec|Identity)' -count=1` exit 0; `go vet ./internal/runtime/controltarget` exit 0/no output; `git diff --check -- internal/runtime/controltarget` exit 0 and `gofmt -l internal/runtime/controltarget/*.go` no output. Read all own validation/model source for self-review, staged only the three own files, `git diff --cached --check` exit 0, then immediate slice commit.
4. Added compilable permissive codec stubs (`json.Marshal`/`json.Unmarshal`), then strict field/bounds/canonical tests. Ran exact focused verbose command, captured `task-1-red-codec.log`, saved test result before tail and exited with it: exit 1. Actual count 259 FAIL, 62 PASS, 0 SKIP. Failures included permissive missing/null/unknown/case/duplicate behavior and mutated destinations; canonical test failed `encoding escaped HTML or added newline`. Existing validation and maximum typed-field sample were already passing (not claimed as new RED).
5. Implemented strict local schema, Unicode precheck, validated transactional decode and closed/unknown canonical producers. Exact focused verbose command exit 0; `task-1-green-codec.log`: 326 PASS, 0 FAIL, 0 SKIP (7 top-level tests +319 subtests).
6. Before slice 2 commit: `go test -race ./internal/runtime/controltarget -run '^TestJournal(Codec|Identity)' -count=1 -v` exit 0, log `task-1-race-codec.log`, 326 PASS/0 FAIL/0 SKIP; `go vet ./internal/runtime/controltarget` exit 0/no output; `git diff --check -- internal/runtime/controltarget` exit 0 and `gofmt -l internal/runtime/controltarget/*.go` no output. Read full own codec plus test diff, staged only codec and tests, `git diff --cached --check` exit 0; immediate slice 2 commit.
7. Final fresh exact focused verbose command exit 0; `task-1-final-focused.log`: 326 PASS/0 FAIL/0 SKIP; `git diff --check BASE HEAD -- internal/runtime/controltarget` exit 0 and final `gofmt -l` no output. `git log -2 --oneline` confirmed the two source commits. Final status showed only unrelated/controller-owned tracked document changes.

All complete logs named above reside next to this report, under `.superpowers/sdd/2026-10-07-runtime-target-journal/`, in the own workspace. No compiler warnings, test warnings, skips, package vet findings or formatting findings occurred. Full repository tests/gates deliberately not run per task dispatch; controller owns final integration gates. No filesystem/ownership/root/container/etcd/physical-execution tests are applicable to this pure slice.

## Coverage and size evidence

- Strict mutation coverage: all 15 manifest field occurrences (4 root +8 identity +3 runtime) and all 29 record field occurrences (7 root +19 context +3 runtime), each with missing, null, wrong type, case substitute and duplicate wire; unknown field in each of the 6 object scopes across both shapes. Tests derive fields from complete actual typed fixtures, not from production schemas.
- Typed identity cases cover namespace form/length/segments, opaque UTF-8/control/size, segment size, lowercase hash, positive generation and all runtime members. Record semantic cases exercise every context field, canonical/nonnil UUID variants, hashes, positive counters, UTC/zero values, equal/reversed/30s+1ns windows and business expiry violation. Exactly 30s and NA==ExpiresAt are valid fixtures.
- Both decoders accept a valid wire padded with whitespace to exactly 8192 bytes; reject 8193 including whitespace. Reject empty/null root, trailing object/garbage, malformed UTF-8, isolated high/low UTF-16 escaped surrogate, integer floats/exponents/int64 and uint32 overflow, wrong Version and depth-9 input. Current schema valid maximum depth is 3; deeper objects reject at the incompatible/unknown field before traversal, with recursive schema depth additionally capped at 8. No assertion that a valid depth-8 journal shape exists.
- Failure destination copying checked for all malformed field/bounds input and invalid times; candidate is assigned only after schema, typed decode and semantic validation succeed. Roundtrip fixture equality includes all scalar/context/time fields; modifying returned copied context leaves original record unchanged.
- Canonical roundtrip is checked, HTML `<` is unescaped, output has no newline; changing NB by 1ns changes complete canonical bytes. Producer rejects open gate, invalid identity and completed record; historical decoder accepts open.
- Actual maximum-legal-field structural sample: record 3280 bytes, manifest 2373 bytes, namespace 512 bytes, timestamps 30 characters (`9999-12-31T23:59:59.999999999Z`). Sample uses every bounded opaque field at 128 bytes of quotes (2x JSON escaping), identifiers at 128 bytes, namespace at 512 with final segments 128, all applicable int64 values at MaxInt64, fixed UUID/hash lengths and full nanosecond precision. This structural sample is separate from the tested 8192-byte whitespace lexical acceptance bound.

## Self-review and limits

Self-review checked exact model fields/tags against binding context and existing protocol; private schema enumerates all existing ExecStartContext fields; no field omission, payload/capability or mutation-on-error found. Scalar ownership and canonical comparison requirements reviewed. Error chains preserve record classification. Invalid Unicode precheck mirrors the proven existing protocol mechanism locally, avoiding protocol edits. Unencodable time years reject before production encode.

One navigation error: initial `rg --files ... internal/runtime/controltarget internal/runtime/controlprotocol` returned exit 2 because target package did not yet exist; protocol file inventory still printed. Created only the authorized target directory afterward. No other helper/setup/navigation/build errors occurred. Expected RED failures and the first masked outer-shell exit are disclosed above. A test map initially specified surrogate replacement against unescaped authority text, then replaced those entries using the actual `json.Marshal` HTML escape sequence before the first RED run; only corrected fixtures were executed. Python helper edits, mkdir, gofmt, all commits and field-count/log-count helpers completed without errors.

No receiver `Journal`, constructor, filesystem access, durable writes, owner validation, flock, fsync, accounting/poison behavior, current identity comparison, authentication, authority establishment, clock calls, lease checks, target launch, replay or physical outcome proof implemented. Overall phases 1–5 remain incomplete. No known Task 1 correctness concerns; independent controller spec and quality gate still required before Task 2.

## Complete saved task-1-review.md

### Spec Compliance

- ✅ **Spec compliant for Task 1.** No missing, extra, or misunderstood Task 1 behavior found in BASE `9fd10b4353b39e36312f899501f64e146c6241f3` through HEAD `9d46041`. The exact persisted identity/manifest/record models, nonwire options/status, and eight sentinel errors are present at `internal/runtime/controltarget/journal_types.go:14`, `:25`, `:36`, `:45`, `:55`, and `:64`. The three value validators are present at `journal_validation.go:14`, `:23`, and `:33`; all shape failures classify as `ErrInvalidRecord`.
- ✅ Identity and context semantics match the inherited protocol: ASCII segment/namespace grammar, bounded UTF-8 opaque fields without control characters, lowercase hashes, canonical nonnil UUIDs, and positive generation/gate/revision/Lease values (`journal_validation.go:15`, `:39`, `:48`, `:63`, `:75`, `:91`, `:102`). UTC nonzero JSON-encodable times and `NB < NA <= ExpiresAt`, with at most 30 seconds, are enforced at `journal_validation.go:39`, `:42`, and `:105`.
- ✅ The controller's explicit producer clarification is implemented: `encodeGateManifest` only produces closed manifests (`journal_codec.go:44`), while `GateManifest.Validate` and decoding accept valid historical open/closed (`journal_validation.go:23`; `journal_codec.go:53`). Unknown is the only valid record state (`journal_validation.go:39`). No receiver, constructor, filesystem, execution, accepted/terminal state, authority, or replay functionality was added; the entire product diff consists of the four authorized metadata/codec/test files.
- ✅ The complete nested schemas enumerate every required exact key, reject duplicates, missing/null/unknown/case-substituted keys, reject invalid UTF-8 and isolated escaped surrogates, enforce integer types and canonical UTC time strings, reject trailing tokens, and impose 8192-byte/depth-8 limits (`journal_codec.go:13`, `:23`, `:100`, `:121`, `:190`). Typed decoding happens only after schema checks; each public decoder assigns its temporary value only after semantic validation (`journal_codec.go:64`, `:84`). Canonical production uses `SetEscapeHTML(false)`, fixed struct order, and no trailing newline (`journal_codec.go:90`). The typed record has only digest/context/time fields, so it cannot persist full tickets or command payloads (`journal_types.go:45`).
- ✅ Every requested product file is present, including the explicitly permitted same-responsibility `journal_validation.go`. The only additional changed file is the controller's binding spec clarification, included in the supplied review package. No protocol or dependency edit appears in the package.
- ⚠️ **Deferred requirements cannot be verified from this Task 1 diff:** current journal identity/epoch binding and authenticated evidence consumption; protected directory/FD construction, ownership, permissions, symlink/hardlink defenses, flock, durable writes, poison/accounting/capacity behavior, nil/zero Journal receivers, and physical authority/outcome boundaries at integration time. These require the designated Task 2/3 and later gates; their absence is not a Task 1 defect. The full production Redis migration and phases 1–5 remain incomplete.

### Strengths

- Transactional decode is simple and reliable: the manifest and record destinations cannot be altered by schema, typed, or semantic failures (`journal_codec.go:64`, `:84`). Local strict validation avoids changing the existing protocol's public API (`journal_validation.go:14`).
- Producer and history-consumer boundaries are explicit both in implementation and tests: historical open fixtures use typed `json.Marshal`, whereas the production encoder rejects open (`journal_codec.go:44`; `journal_codec_test.go`, `TestJournalCodecCanonical`).
- The tests discover nested fields from real typed fixtures rather than sharing the production schema, giving the required-field checks an independent oracle (`journal_codec_test.go:114`, `TestJournalCodecStrictFields`). Existing saved runs contain 326 passing nodes and no skipped nodes, including malformed-wire destination preservation, lexical limits, real roundtrips, canonical encoding, payload exclusion, and time/window cases (`task-1-final-focused.log:654`).
- Maximum legal typed-field sample evidence is concrete: record 3280 bytes, manifest 2373 bytes, namespace 512 bytes, and a 30-character timestamp (`journal_codec_test.go:390`; `task-1-final-focused.log:652`). This is correctly distinguished from acceptance of a valid document padded to the 8192-byte lexical bound.
- Responsibilities remain clear across 72-line models, 106-line validators, 231-line codec, and 398-line tests; the task introduces no background activity or storage side effects (`review-9fd10b4..9d46041.diff:9`).

### Issues

#### Critical (Must Fix)

- None.

#### Important (Should Fix)

- None.

#### Minor (Nice to Have)

- None identified.

### Checks and Evidence Limits

- Read the complete 866-line/37986-byte supplied review package once in bounded consecutive ranges 1–220, 221–440, 441–660, and 661–866. Read the complete task brief, typed context, binding review context, and worker report. Recovered the worker report's truncated initial tool display by bounded reads; no tool truncation was treated as EOF. No changed source file was separately reread, and no git command was run.
- **Named unchanged-code risk: inherited protocol shape/tag drift.** One focused symbol search located the exact existing models/validators, followed by reads limited to `controlprotocol/exec_start_types.go:7–27`, `publication_types.go:11–15`, `exec_start.go:118–138`, and `publication_verify.go:209–266`. The 19 context fields, three runtime fields, opaque/segment/namespace/hash/UUID predicates, positive counters, and UTC/window rules match. The journal's additional year bound supports the required encodable canonical time form. No broader codebase or call-site crawl was needed for this purely new passive codec.
- Read only the five named saved test logs to check their complete stored contents, outcomes, counts, size samples, and warning/build/panic/race markers. Validation RED: 61 FAIL/0 PASS/0 SKIP (`task-1-red-validation.log:181–183`). Codec RED: 259 FAIL/62 PASS/0 SKIP (`task-1-red-codec.log:901–903`). Codec GREEN, recorded race run, and final focus each show 326 PASS/0 FAIL/0 SKIP and successful package completion (`task-1-green-codec.log:654–655`, `task-1-race-codec.log:654–655`, `task-1-final-focused.log:654–655`). No warning, data-race, build-failure, undefined-symbol, or panic markers were found. These are inspected prior-run artifacts, not independently rerun tests.
- No test, suite, race detector, vet, formatting, or build command was rerun. The raw standalone vet/gofmt/diff-check command output and transient stub states are not contained in the supplied diff or saved logs; their execution details remain worker-reported (`task-1-report.md:51–57`). The saved behavioral RED/GREEN evidence is readable and consistent with the reviewed implementation. No additional validation was required by a concrete unresolved code risk.
- Review was read-only except creation of this expressly requested report. No source, index, HEAD, branch, unrelated Sentinel document, user configuration, credential, ignored content, or sibling workspace was read or mutated.

### Assessment

**Task quality:** Approved.

**Reasoning:** The typed passive wire boundary meets the exact Task 1 contract and uses straightforward schema-first, transactional decoding with independent malformed-field tests. No blocking correctness, scope, or maintainability issue was found; approval is confined to Task 1 and does not establish filesystem durability, runtime authority, or completion of the production migration.

**Finding counts:** Critical 0; Important 0; Minor 0.

## Complete saved task-2-report.md

# Task 2 implementation report

Status: DONE. Source frozen at `f07450c9ff7ca9e21277e9c61d88b32411314b3b`, base `64632d67888942300dba6be509e8bce21056acec`. Independent review remains Root-owned. This report covers only Task 2, not completion of the overall etcd migration.

## Scope and commits

- `5ac13c3` feat(controltarget): protect journal file reads with pinned Unix directories
- `6547f2b` feat(controltarget): durably close gates and validate retained journal history
- `2039516` feat(controltarget): add immutable command file primitives and recovery fault checks
- `f07450c` test(controltarget): verify bounded file slots and pinned journal recovery

Created `internal/runtime/controltarget/journal.go`, `journal_files_unix.go`, `journal_files_unsupported.go`, `journal_accounting.go`, `journal_files_test.go`, `journal_gate_test.go`, `journal_accounting_test.go`. No Task 1 codec/type/protocol changes, public RecordUnknown/Lookup, etcd/Manager/config/legacyruntime/image/dependency changes, or production wiring. The pre-existing Sentinel plan edit remains untouched and unstaged. No subagents or Docker lifecycle actions by this worker.

Implemented actual directory FDs, component-by-component no-follow parent pinning, canonical absolute input path, exact current effective ManagementUID, 0700 directories and 0600 regular single-link files. Open flags include O_NOFOLLOW/CLOEXEC and O_NONBLOCK for untrusted file types, preventing FIFO blocking. Real lifetime flock rejects another opener and releases on Close/process hard exit. Create refuses existing targets and never repairs/deletes partial directories.

The public constructors and CloseGate produce only durable closed gates. Open verifies exact persisted identity/epoch, all retained content and accounting before closing historical open. Nil/zero handles and nil contexts follow the binding context. Close/Status share the journal mutex; Close releases descriptors, preserves evidence and is idempotent. Unsupported platforms reject constructors explicitly with ErrInvalidConfiguration.

Persistence is exclusive temp creation, complete write, file fsync, same-directory rename, directory fsync. New directory creation fsyncs child and containing directory; Create additionally syncs the immediate parent last. On uncertain persistence, the handle becomes Poisoned, AccountingKnown=false and NewRecordsStopped=true; counters remain the last verified snapshot. No temp/evidence cleanup is attempted. No receipt succeeds after an injected failure or cancellation at any tested write boundary.

Cold recovery uses Readdirnames(128), with separate level scans so parent pages are not retained during bucket scans. It validates commands directories first and then enumerates the fixed 256 possible bucket names without retaining a map/list. Every actual history file is bounded to 8192 bytes and decoded/bound. Missing manifest cannot be inferred from a temp. Unknown names, malformed/unsafe files, wrong bucket/ID/binding or incomplete temps fail closed. Valid temps remain and are counted. No record/history cache, idle goroutine, lease/watch/connection, replay, open transition or GC exists.

## Exact private consumer APIs for Task 3

All methods ending in Locked below require the caller to hold `j.mu`; they do not acquire it. Task 3 public methods must handle `ctx == nil` and `j == nil` before acquiring `j.mu`, then defer unlock. Never copy a live Journal/mutex.

```go
func (j *Journal) checkLocked(ctx context.Context, write bool) error
func (j *Journal) readCommandLocked(ctx context.Context, id string) (*ExecJournalRecord, error)
func (j *Journal) persistNewCommandLocked(ctx context.Context, r ExecJournalRecord) error
func (j *Journal) recordBinding(r ExecJournalRecord) error
func (j *Journal) poison(err error) error
```

- `checkLocked`: nil ctx -> ErrInvalidConfiguration; nil/uninitialized Journal -> ErrJournalUnavailable; closed initialized handle -> ErrClosed; write on poisoned handle -> ErrJournalUnavailable; otherwise checks ctx.Err. A context already cancelled before entering persistence returns its error without an attempted write. A cancellation observed during persistence poisons. Reads on poisoned handles remain allowed and must validate disk.
- `readCommandLocked`: calls checkLocked(ctx,false), validates canonical nonnil UUID, opens only its fixed bucket with a pinned/verified directory FD, then its fixed `<id>.json` file with no-follow protection; verifies owner/mode/type/single-link, stats size before bounded read, strict-decodes, validates filename ID and all journal identity/epoch fields. Missing bucket/file returns nil,nil. Returned scalar record is owned; nothing is cached. Other filesystem and validation errors preserve their causes. It does not poison solely because a read fails. Closed/nil/zero caller semantics are checked before filesystem access.
- `persistNewCommandLocked`: calls checkLocked(ctx,true), canonical-encodes/validates the complete record and checks binding, then performs a real read of the same ID. ANY existing valid record returns ErrConflict (including identical content); it NEVER overwrites. Task 3 owns authentic evidence conversion and complete canonical same-record retry comparison, before new-record capacity enforcement. Task 3 should first check write availability, then read/compare same ID, and only call this helper for absence. Do not interpret private record acceptance as cryptographic evidence or launch authority.
- New-record capacity is checked before any new mutation: unknown accounting -> ErrJournalUnavailable; `(LogicalBytes + encodedBytes)*100 >= MaxBytes*85` -> ErrCapacity; projected content files reaching 65536 -> ErrCapacity, preserving a gate temp slot. Same-ID retries are to be handled by Task 3 before this path. The helper obtains a random canonical UUID nonce, opens/creates the bucket, syncs a newly created bucket plus commands, persists the immutable command via real syscalls, then increments Records/LogicalBytes only after full success. Any error after starting bucket/file mutation conservatively poisons and preserves all possibly committed bytes. A returned error can coexist with a complete final command; Task 3 must not treat it as proof that no intent exists or a command failed.
- `recordBinding` compares Namespace, AuthorityID, Target, RestoreEpoch, SandboxID, WorkspaceHash, Generation, exact Runtime(ID/UID/BootID), and DataGateEpoch against j.gate; mismatch -> ErrIdentityMismatch. Semantic record validation remains the codec's job before this helper.
- `poison` sets poisoned=true/accountingKnown=false and wraps both ErrJournalUnavailable and original error. Only cold reopen recomputes trustworthy counters.

Private supporting constructor `newJournal(ctx, options, create bool, hook journalIOHook)` exists for synchronous real-IO testing; production public constructors pass nil hook. `journalIOHook` is `func(operation, name string, after bool) error`. Operations are `mkdir`, `open-temp`, `write`, `file-sync`, `rename`, `dir-sync`, `read-page`; `after=false/true` bracket real operations, with context checks before and after. Hooks never provide a fake persisted store. Hooks are installed before use or changed only without concurrent access; async-test cleanup joins before restoring them. `openChild`/point read syscalls are not separately hook-instrumented in Task 2; Task 3 can add synchronous syscall observation for its 0/1000 hotpath-count test if needed.

Actual point reads are fixed-depth openat+fstat for bucket, openat+fstat/stat+bounded read for file, and descriptor closes. New immutable writes also perform an absence point read, bucket open or durable mkdir, temp open/write/fsync/rename/dir-fsync. They never call walkPage/Readdirnames. This task measures 1000-history cold paging, not a Task 3 0/1000 point-operation equality claim.

## Verification commands and actual results

All logs listed below are in `.superpowers/sdd/2026-10-07-runtime-target-journal/`. Counts are all `--- PASS/FAIL/SKIP` nodes including aggregate parent subtests, not an inflated claim of independent test cases. Non-verbose package/build/vet outputs do not enumerate test nodes. All Go commands ran from `/Users/dysodeng/project/go/cloud/sandbox`.

| Command | Actual exit | PASS / FAIL / SKIP | Complete log |
|---|---:|---:|---|
| `go test ./internal/runtime/controltarget -run '^TestJournalProtectedFiles$' -count=1 -v` (compiling stub RED) | 1 | 0 / 3 / 1 | task-2-protected-red.log |
| Same selector, protected IO GREEN | 0 | 9 / 0 / 1 | task-2-protected-green.log |
| `go test ./internal/runtime/controltarget -count=1` (slice 1) | 0 | package PASS; not verbose | task-2-slice1-package.log |
| `go test ./internal/runtime/controltarget -run '^TestJournalGate$' -count=1 -v` (compiling stub RED) | 1 | 0 / 5 / 0 | task-2-gate-red.log |
| Same selector, gate GREEN | 0 | 5 / 0 / 0 | task-2-gate-green.log |
| `go test ./internal/runtime/controltarget -run '^TestJournalPersistenceFaults$' -count=1 -v` (recovery RED) | 1 | 18 / 5 / 0 | task-2-fault-red.log |
| `go test ./internal/runtime/controltarget -run '^TestJournal(Recovery|PagedAccounting)$' -count=1 -v` (recovery RED) | 1 | 5 / 11 / 0 | task-2-recovery-red.log |
| `go test ./internal/runtime/controltarget -run '^TestJournal(Gate|Recovery|PersistenceFaults|PagedAccounting)$' -count=1 -v` | 0 | 44 / 0 / 0 | task-2-gate-recovery-green.log |
| `go test ./internal/runtime/controltarget -count=1` (slice 2) | 0 | package PASS; not verbose | task-2-slice2-package.log |
| `go test ./internal/runtime/controltarget -run '^TestJournalCommand(Primitives|Capacity)$' -count=1 -v` (compiling stub RED) | 1 | 0 / 2 / 0 | task-2-command-red.log |
| Same command, point primitives GREEN | 0 | 2 / 0 / 0 | task-2-command-green.log |
| `go test ./internal/runtime/controltarget -run '^TestJournal(ProtectedFiles|PersistenceFaultsWorker|PagedAccounting)' -count=1 -v` | 0 | 29 / 0 / 2 | task-2-protected-worker-green.log |
| `go test ./internal/runtime/controltarget -run '^TestJournal(PersistenceFaultsWorker|PersistenceFaultsDirectoryChain|RecoveryHardBudget)$' -count=1 -v` | 0 | 17 / 0 / 0 | task-2-additional-green.log |
| `go test ./internal/runtime/controltarget -run '^TestJournalPersistenceFaultsWorker$/fatal-cleanup-regression$' -count=1 -v` (deliberate missing-release mutation) | 1 | 0 / 2 / 0 | task-2-fatal-cleanup-red.log |
| `go test ./internal/runtime/controltarget -run '^TestJournal(Command|PersistenceFaultsWorker)' -count=1 -v` (restored cleanup) | 0 | 14 / 0 / 0 | task-2-command-fatal-green.log |
| `go test ./internal/runtime/controltarget -count=1` (slice 3) | 0 | package PASS; not verbose | task-2-slice3-package.log |
| `go test ./internal/runtime/controltarget -run '^TestJournal(ProtectedFilesPinnedRoot|RecoveryCancelledScan)$' -count=1 -v` | 0 | 2 / 0 / 0 | task-2-pinned-cancel-green.log |
| `go test ./internal/runtime/controltarget -run '^TestJournalPagedAccountingFileLimit$' -count=1 -v` | 0 | 1 / 0 / 0 | task-2-file-limit-green.log |
| Final focused command below, first attempt with bad metadata-test assertion | 1 | 15 / 1 / 0; panic | task-2-metadata-helper-failure.log |
| Final focused command below, corrected helper | 0 | 89 / 0 / 2; 12 top-level PASS; 13.544s | task-2-final-focused.log |
| `go test -race ./internal/runtime/controltarget -count=1 -v` | 0 | 428 / 0 / 2; 24 top-level PASS; 28.854s | task-2-final-race.log |
| `go vet ./internal/runtime/controltarget` | 0 | empty output | task-2-final-vet.log |
| `gofmt -l internal/runtime/controltarget/journal*.go` | 0 | empty output | task-2-final-format.log |
| `git diff --check` and staged diff checks before each commit | 0 | empty output | task-2-final-diff-check.log |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c ./internal/runtime/controltarget -o /tmp/task-2-unsupported-windows.test.exe` | 0 | compile only, not execution | task-2-unsupported-build.log |
| `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/runtime/controltarget -o .superpowers/sdd/2026-10-07-runtime-target-journal/task-2-linux-arm64.test` | 0 | compile only, not execution | task-2-linux-build.log |

Final focused command: `go test ./internal/runtime/controltarget -run '^TestJournal(ProtectedFiles|Gate|Recovery|PersistenceFaults|PagedAccounting)' -count=1 -v`. Final race covers the complete package, including codec tests and private command tests. No whole-repository test claim; no unrelated broad suites run.

TDD REDs were compiling behavioral assertions: protected reader/parent stubs returned unavailable for safe files; gate stubs failed lifecycle/receipt tests; missing recovery accepted incomplete temps/unknown names/bad binding and reported zero history; command stubs failed real persistence/capacity tests. The corresponding GREEN logs demonstrate actual filesystem behavior. Additional coverage for already-implemented syscall protection is not claimed as a separate new-feature RED cycle.

One helper error is explicitly retained: the first final focused attempt asserted `os.FileInfo.Sys().(*unix.Stat_t)`, but the actual value is `*syscall.Stat_t`, causing a panic in test-only metadata reporting. It was corrected to real unix.Lstat and the same full focused command passed. No compile failures or production failures were hidden as TDD evidence. No final compiler/vet/race warnings occurred.

## Linux UID0 ownership gate performed by Root

Read and referenced Root's `task-2-root-linux-report.md`. Source exactly f07450c. Root independently inspected the pinned local image architecture as arm64, verified ELF machine183 and binary SHA256 `cd8666c1186dd4f968a7e5711a2a9908fed54b198c01fecaa6e3102b67b20e4e`.

Actual Root command: `python3 .superpowers/sdd/2026-10-07-runtime-target-journal/root-linux-fixture.py .superpowers/sdd/2026-10-07-runtime-target-journal/task-2-linux-arm64.test '^TestJournalProtectedFiles' --tag task-2`.

Actual tool/test exit 0 confirmed by docker attach/inspect/wait: 3 top-level / 34 PASS nodes, 0 FAIL, 0 SKIP, 0 warnings. Full evidence: `task-2-root-linux.log` and `task-2-root-linux.json`. Real owner/chown checks ran for parent/files/root/commands/bucket/gate/lock/record; root substitution check also ran. No mocked stat or host-skip substitution.

Fixture project `sandbox-target-journal-test-63970-1791320212192913000`: UID0, network none, drop ALL + CHOWN only, no DAC_OVERRIDE, NNP, binary readonly bind, test-owned /tmp volume. It ran no etcd process and had no host writable/credential mount. Root's exact owned container and volume removal each exited 0; final label inventories of containers/volumes/networks all 0. This is native Linux ProtectedFiles execution only, NOT Linux race or the complete Linux package. The two host chown skips remain honestly reported separately. Complete Linux package execution remains a later gate as specified for Task 3.

## Fault retention, cancellation and worker cleanup

Gate fault matrix tests error and ctx cancellation both before and after real temp-open/write/file-fsync/rename/dir-fsync (20 combinations). Before temp-open: no new temp. After temp-open or before write: actual empty temp retained; cold reopen fails. After complete write/file-fsync or before rename: complete temp retained; recovery validates/counts it without deletion. After rename or at dir-fsync: possible committed final gate remains, but original call returns zero receipt + error and poisons. Counters are never presented as known after uncertainty. A fresh successful reopen recomputes them.

Command fault matrix covers before/after write/file-fsync/rename/dir-fsync. A possible committed command is readable on the poisoned handle after full real validation and equals the original record; uncommitted complete temps remain temporary history, and incomplete command temp causes recovery failure. New writes on poison fail. Cold-recovery cancellation preserves bytes and releases the lock.

All six actual create directory-sync positions are checked with failures before and after; each failure returns nil journal, preserves already-created evidence and refuses overwrite. A successful Create observes the exact sequence root, parent, commands, root, root-for-gate-rename, final-parent. Real file count test creates 65534 retained gate temps plus manifest, verifies reserved gate temp slot works while a new command is refused, and verifies a 65536-file store cannot acknowledge another gate temp write. Hard byte budget also fails close without deleting evidence.

The ordinary subprocess helpers are synchronous `exec.CommandContext` with a 5s context and 1s WaitDelay; CombinedOutput joins the child before returning, so Fatal cannot leave a caller-owned wait goroutine. Busy-child normal exit and hard-exit code23 are deliberate and checked. TestJournalProcessHelper returns normally when its explicit env trigger is absent.

The blocking file-sync worker uses a real gate operation and explicit entered/release/done channels. Cleanup is registered before a caller can Fatal: cancel context, release once, join with <=5s timeout, then restore hook. Fixture FD cleanup is registered earlier and therefore runs after that join. The ordinary cancellation case observes context.Canceled, poison and no false receipt. The real Fatal subprocess intentionally exits 1; its parent confirms the Fatal marker and `WORKER_JOINED_AND_LOCK_RELEASED`, and cleanup additionally reopens the retained store after closing to prove flock release. Deliberately removing release made this parent test fail at 5s with the child killed; restoration passed. These expected child exits are not hidden suite failures.

## Self-review, resource measurements and limitations

Reviewed my own source and staged diffs before coherent commits. Checked pathname-only validation is never used instead of directory-FD opens; every externally persisted content read is bounded; directory pages are not nested/cached; gate temp capacity uses hard bytes/count whereas new command uses soft threshold/reserved slot; same-ID content is never replaced; uncertain writes preserve evidence; errored constructors close their actual FDs/flock; context/nil/closed paths do not dereference unusable handles; source scope stayed inside the package.

Self-review and tests led to additional assertions for pinned root substitution, owner restoration with CHOWN-only privileges, hard byte/file capacity, cancelled scan lock release and Fatal cleanup. The test metadata panic was fixed as disclosed above. No unresolved implementation concern identified; independent spec/quality review remains Root's gate.

At 1000 synthetic commands on the host filesystem, measured LogicalBytes=1,110,361; 3 directories; reported allocated directory blocks=0 bytes; 1002 regular files including lock with 4,100,096 allocated bytes. Zero reported directory blocks does not mean zero directory/inode overhead. The journal retains 3 steady descriptors (root, commands, lock), with transient parent/bucket/file descriptors. These are local filesystem observations, not a resource quota or physical isolation guarantee. Paged recovery observed 13 actual calls: 2 root + 2 commands + 9 bucket Readdirnames(128), including EOF pages. No all-history in-memory map exists.

Fsync on the supported local filesystem plus process close/reopen/hard exit is tested. No power-loss/host-failure/hardware durability claim, no actual PID1 launch/drain proof, and no immediate cancellation guarantee for a blocked OS fsync. Startup scan scales with this runtime's own history; the fixed 256-name bucket loop is not a claim that total recovery is constant. The synthetic 1000-record and 65536-file tests are not Pod count, QPS, p99, retention/GC or whole-migration validation. No authority is reconstructed from history, closed receipts, cancellation or unknown intents.

## Artifact copy incident and cleanup disclosure

Root requested preserving this task's /tmp logs under its own SDD directory. I mistakenly used a broad `Path('/tmp').glob('task2-*.log')` copy loop, which also copied 15 pre-existing unrelated /tmp log files. These were NEW destination files in this task's directory, not overwrites of prior evidence: inspected birthtimes for copied files were 2026-10-07 04:54:01.965...–04:54:01.968..., the copy operation, while the task brief was created at 04:26:40. A later summary script counted PASS/FAIL markers of those copies before the mistake was noticed; no unrelated raw contents were output. This was an avoidable scope mistake, not part of the implementation evidence.

The unintended sources were `/tmp/task2-` plus these exact suffixes: `full-package.log`, `hardening-green.log`, `ttl-red.log`, `race.log`, `red.log`, `fixround1-green.log`, `unit1-tests.log`, `fixround1-positive-green.log`, `unit2-tests.log`, `fixround1-positive.log`, `revision-red.log`, `green.log`, `fixround1-red.log`, `unit3-tests.log`, `hardening-red.log`. The corresponding accidental new destination copies used prefix `task-2-`. Cleanup unlinked only destination copies whose birthtime was within 1 second of the known copy operation and excluded the explicit six own source logs. All 15 accidental destinations were removed; original /tmp files were not modified or removed. No source files, credentials or user configuration were affected. This cleanup and the copying mistake were reported to Root immediately. Subsequent evidence uses only explicit owned filenames; none of those 15 logs is used to substantiate any claim in this report.

## Fix round 1 — Important I1 Fatal-cleanup result contract

Received independent review `task-2-review.md`: Spec issue / Quality Needs fixes, C0 I1 M1. Verified I1 directly: the helper's intentional Fatal already yields exit1, so later t.Error diagnostics plus an unconditional success marker allowed its parent to accept additional failed cleanup postconditions. This was a real test-oracle defect; prior success did not establish those postconditions. Only I1 is addressed here. Minor M1 retained-unknown hard-exit coverage remains explicitly deferred by Root to Task 3's actual public-intent restart tests.

Fix base `f07450c9ff7ca9e21277e9c61d88b32411314b3b`; new frozen HEAD `7fcbd994fcb40e70d6ba144ea8bd4a0accf53a2d`. Commit `7fcbd99 test(controltarget): require every Fatal cleanup postcondition to succeed`. Only `internal/runtime/controltarget/journal_gate_test.go` changed (61 insertions, 17 deletions). Production source, private/public APIs, fixture platform constraints, and original Linux binary are unchanged.

The Fatal helper now tracks an explicit `cleanupOK` independent of the already-failed `testing.T`. Every failed required child postcondition marks that result false: closing the original journal, observing poison, reading/finding retained evidence, reopening to verify retained bytes and flock release, and closing the reopened handle. The accepted `WORKER_JOINED_AND_LOCK_RELEASED` marker is emitted only when that complete result is true. The parent runs both a complete cleanup and a deliberately broken non-join cleanup, with opposite marker expectations; both still require the intentional Fatal diagnostic and expected child exit1.

The persistent negative regression actually removes the retained `.gate.*.tmp` file from the test-owned journal AFTER the actual worker cleanup has cancelled, released, joined and restored its hook. It requires a distinct `MUTATED_RETAINED_EVIDENCE_REMOVED` marker, the real `uncertain evidence removed` diagnostic, and absence of the cleanup success marker. Failure to set up that real mutation itself makes the test fail. No fake stat/store and no production mutation is used. The pre-existing cancellation/release/join order and 5s child context/1s WaitDelay remain intact. The successful positive case proves intentional Fatal alone does not suppress a genuine cleanup-success result; the new negative case proves a non-join postcondition failure does suppress it.

Exact commands, actual exits and preserved owned logs:

| Command | Actual exit | Result | Log |
|---|---:|---|---|
| `go test ./internal/runtime/controltarget -run '^TestJournalPersistenceFaultsWorker$/fatal-cleanup-regression$/missing-retained-evidence$' -count=1 -v` before the result fix | 1 | Compiling behavioral RED: `cleanup success marker=true want=false`, real removed-evidence mutation, original success marker and failed retained-evidence diagnostic all visible. 3 outer FAIL nodes plus 1 captured intentional child FAIL node; 0 SKIP. | task-2-i1-red.log |
| `go test ./internal/runtime/controltarget -run '^TestJournal(PersistenceFaultsWorker|FatalCleanupHelper)$' -count=1 -v` after fix | 0 | 2 top-level / 6 inclusive PASS, 0 FAIL, 0 SKIP; package 0.586s. | task-2-i1-green.log |
| `go test -race ./internal/runtime/controltarget -run '^TestJournal(PersistenceFaultsWorker|FatalCleanupHelper)$' -count=1 -v` | 0 | 2 top-level / 6 inclusive PASS, 0 FAIL, 0 SKIP; no race warnings; package 1.610s. | task-2-i1-race.log |
| `go vet ./internal/runtime/controltarget` | 0 | Empty output, no warnings. | task-2-i1-vet.log |
| `gofmt -l internal/runtime/controltarget/journal_gate_test.go` | 0 | Empty output. | task-2-i1-format.log |
| `git diff --check -- internal/runtime/controltarget/journal_gate_test.go` and `git diff --cached --check` before commit | 0 | Empty output; scoped source diff fully self-reviewed. | task-2-i1-diff-check.log |
| `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/runtime/controltarget -o .superpowers/sdd/2026-10-07-runtime-target-journal/task-2-fix-1-linux-arm64.test` | 0 | New explicit owned binary; compile only, not native execution. No build/helper failure. | task-2-i1-linux-build.log |

There were no setup/compiler failures, unexpected skips, warnings or hidden failing test attempts in this round. The intended RED is retained unchanged; GREEN and race have separate logs. No full 65536-file suite or unrelated broad suite was repeated for this test-only correction. Only exact owned filenames were read/written; no /tmp glob, unrelated artifacts or Root document modifications occurred.

Self-review: read the complete scoped diff; checked every prior t.Error postcondition now updates cleanupOK; checked the reopened Close result is also required; checked the negative marker cannot appear without actually removing a retained file; checked the parent demands failure-specific diagnostics instead of accepting an arbitrary subprocess failure; checked both children are joined by synchronous CombinedOutput before the caller can Fatal. This is implementer self-review only, not independent approval. Root owns native fixture evidence and scoped re-review.

New binary and frozen HEAD were sent to Root for actual Linux selector `^TestJournal(ProtectedFiles|PersistenceFaultsWorker)` using the distinct `task-2-fix-1` fixture/evidence tag. Native result is appended below when supplied by Root; prior native ProtectedFiles evidence remains associated only with its original source/hash.

Fix 1 native result received and Root report read: `task-2-fix-1-root-linux-report.md`, full raw log `task-2-fix-1-root-linux.log`, metadata `task-2-fix-1-root-linux.json`. Root verified actual frozen HEAD `7fcbd994fcb40e70d6ba144ea8bd4a0accf53a2d`, ELF machine183, new binary SHA256 `c50e958056b878a5066fd565666afd7ee5110bcc027b4495dd0a4b11ec6de8b3`, and preserved the original binary.

Root executed `python3 .superpowers/sdd/2026-10-07-runtime-target-journal/root-linux-fixture.py .superpowers/sdd/2026-10-07-runtime-target-journal/task-2-fix-1-linux-arm64.test '^TestJournal(ProtectedFiles|PersistenceFaultsWorker)' --tag task-2-fix-1`. Actual fixture/test exit0 confirmed by attach/inspect/wait; 4 top-level / 39 PASS nodes, 0 FAIL, 0 SKIP, 0 warnings/panics. The actual Linux run includes both Fatal cleanup success and non-join negative mutation branches, plus actual ownership/CHOWN checks. This is native execution, not Linux race/full package.

Root fixture project `sandbox-target-journal-test-65959-1791321154461169000` used network none, UID0, drop ALL + CHOWN only, NNP, readonly binary and owned /tmp volume. Exact owned container and volume removal each exited0; final labelled container/volume/network inventories were all0. No worker Docker lifecycle operation occurred.

Fix round 1 implementation status: DONE, I1 corrected and scoped verification complete; Root's independent scoped re-review remains pending. No unresolved I1 implementation concern. M1 remains deliberately deferred to Task 3 as instructed. No further source changes after the verified freeze.

## Complete saved task-2-review.md

# Spec Compliance

❌ Issues found: Task 2's requested real Fatal-path regression does not reliably check all of the cleanup postconditions it claims to prove. Its child reports additional failures with `t.Error` but still prints the success marker, and its parent accepts the already-expected exit 1 plus that marker (`internal/runtime/controltarget/journal_gate_test.go:382`, `:394`, `:417`). See Important I1. The production implementation otherwise matches the Task 2 contracts visible in this package; no public RecordUnknown/Lookup was added.

⚠️ Cannot verify from this diff: complete Linux package native acceptance, Task 3 authentic-evidence producer/canonical same-record retry/0-versus-1000 point-operation counts, actual target/PID1 isolation and launch/drain, power-loss durability, physical resource quotas, retention/GC, and overall migration. Controller should retain these later gates; Task 2 ProtectedFiles native execution and host race do not substitute for them (`task-2-context.md:87`, `task-2-report.md:91`, `:108`). This is a task-scoped review, not a whole-branch merge approval.

# Strengths and verified contracts

- Canonical absolute path is checked; parent aliases are rejected; traversal pins each component with no-follow directory opens and validates the immediate parent's exact owner/mode. Child directories and files use openat, O_NOFOLLOW, CLOEXEC, and NONBLOCK, followed by real fstat owner/type/mode/single-link verification (`internal/runtime/controltarget/journal_files_unix.go:21`, `:60`, `:77`). NONBLOCK prevents the supplied FIFO from hanging the protected reader. The pinned-root substitution test verifies writes stay on the original directory (`journal_files_test.go:221`). All required create-target refusal, symlink/mode/hardlink/owner cases have actual filesystem tests (`journal_files_test.go:29`, `:130`; `journal_gate_test.go:35`).
- Lifetime actual flock is obtained before recovery/persistence and closed on constructor failure or Close. Independent subprocess busy and hard-exit cases exercise actual process locks (`journal_files_unix.go:154`; `journal.go:124`; `journal_gate_test.go:144`, `:162`, `:182`). No PID/mkdir substitute is used.
- Durable persistence orders exclusive temp creation, complete bounded write, file Sync, same-directory Renameat, directory Sync. Directory creation syncs both new child and containing directory; Create syncs the immediate parent again before success (`journal_files_unix.go:133`, `:235`, `:259`). Failure retains possible-committed evidence, poisons the handle and marks accounting unknown; the gate is published to memory only after full success (`journal.go:85`; `journal_files_unix.go:302`). Synchronous hooks bracket actual operations with pre/post context checks (`journal.go:149`), rather than replacing persisted storage.
- Open requires the real manifest, exact identity/epoch, safe commands layout and successful bounded validation before durably writing closed. Historical open is read through the decoder and then closed through the closed-only encoder. Missing gate is not recovered from a temp (`journal_files_unix.go:213`, `:235`; `journal_accounting.go:70`; `journal_gate_test.go:118`; `journal_accounting_test.go:41`). Invalid names, bucket/record binding, malformed temps and unknown entries fail without deleting evidence.
- Recovery uses Readdirnames(128), separate directory-level passes and a fixed 256-bucket enumeration, retaining no whole-history map/list. Every content file is bounded and decoded; valid temps count toward bytes/files. The 1000-file test checks 13 actual page calls, records/bytes and filesystem allocation observations (`journal_accounting.go:16`, `:70`; `journal_accounting_test.go:183`). This is bounded memory, not constant total recovery work.
- Soft 70/85 percent accounting and hard byte/file limits are separate. New-command projected count reaching 65536 is denied to reserve a gate-temp slot; gate writes use hard limits. The real 65534-temp-plus-manifest fixture checks the reserved slot and rejects another close after total persisted count reaches 65536 (`journal.go:112`; `journal_files_unix.go:302`, `:389`; `journal_accounting_test.go:300`, `:344`, `:477`). Counters update only after successful persistence.
- Mutex serialization covers public CloseGate, Status and Close; checks define nil context, nil/zero handle, closed and poisoned behavior, with idempotent Close and a copied status (`journal.go:66`, `:91`, `:112`, `:124`; `journal_gate_test.go:88`). There are no background goroutines/timers in product code. The only new worker is a test worker whose registered cleanup cancels/releases/joins before hook restoration (`journal_gate_test.go:323`); I1 concerns the effectiveness of its subprocess postcondition assertions.
- Private point reads validate UUID, bucket/file metadata, strict content, filename and exact journal binding and return an owned scalar record; writes check absence and cannot overwrite an existing valid command under the mutex/flock ownership assumptions. Reads remain possible on poison; writes do not (`journal_files_unix.go:331`, `:369`; `journal_accounting.go:44`). Task 3's exact helper signatures, lock ownership, nil/zero checks, conflict-versus-identical-retry responsibility and poison behavior are explicitly documented (`task-2-report.md:21`–`:43`). Same-record retry remains Task 3 responsibility rather than being silently approximated here.
- Linux/darwin and unsupported build constraints are complementary, and unsupported constructors explicitly reject configuration (`journal_files_unix.go:1`; `journal_accounting.go:1`; `journal_files_unsupported.go:1`, `:7`). All seven new files stay within controltarget; no controlprotocol, dependency, Manager, configuration, legacy runtime or production wiring changes appear in the complete diff. Files separate handle/state, Unix I/O, accounting and tests; the 413-line Unix implementation remains coherent and composed of focused helpers rather than one large routine.

# Issues

## Critical (Must Fix)

None found (0).

## Important (Should Fix)

I1 — Fatal-cleanup regression swallows failed postconditions. `internal/runtime/controltarget/journal_gate_test.go:394`–`:417` calls `t.Error` for close failure, missing poison, missing retained evidence, and reopen/lock failure, then unconditionally prints `WORKER_JOINED_AND_LOCK_RELEASED`. The subprocess already exits 1 because of the intentional Fatal at `:420`. The parent at `:382` requires only that same exit code, the intentional-Fatal text and the unconditional marker, so all of those additional failures are accepted as a passing parent test and their output is discarded. For example, dropping poison while retaining the temp and successfully returning from cleanup still emits the marker and produces the expected exit 1. This makes a requested verification gate falsely green; it is a test defect, not a demonstrated production persistence defect. Fix by maintaining an explicit cleanup-success result and emitting the accepted marker only when every required postcondition succeeds, or by sending a distinct machine-readable success/failure result that the parent validates. Do not use `t.Failed()` alone because intentional Fatal already sets it. Add a focused deliberate negative mutation for at least one non-join postcondition; the saved missing-release mutation covers only the timeout/kill branch.

## Minor (Nice to Have)

M1 — Hard-exit recovery fixture contains no unknown record. `internal/runtime/controltarget/journal_gate_test.go:144`–`:156` creates an empty journal, makes the child open and `os.Exit(23)` (`:182`–`:199`), and checks only the gate state. Thus this process-restart test proves lock release and gate readability but cannot detect loss/corruption of unknown command bytes. Existing command-fault recovery tests are useful, but they close/reopen in one process (`journal_accounting_test.go:386`). Extend the hard-exit fixture with a real private persisted command, then verify its exact record/bytes and accounting after child exit. This would directly cover the formal retained-unknown/complete-bytes restart acceptance without implying power-loss coverage.

# Saved verification evidence inspected

- Read complete `task-2-root-linux-report.md`, raw `task-2-root-linux.log` (69 lines, 4529 bytes), and `task-2-root-linux.json`. Log has 34 PASS nodes, 0 FAIL, 0 SKIP and 3 top-level tests. JSON records actual arm64 image inspect; UID0, network none, readonly test-binary bind, owned /tmp volume, drop ALL plus CHOWN, no-new-privileges; attach/inspect/wait actual test exit 0; exact owned container/volume cleanup exit 0 and empty final label inventories. This supplies the Task 2 Linux root-only ownership gate. It is native ProtectedFiles only, not Linux race/full package.
- Read final host focused/race logs through a bounded summary of every result marker and warning/panic/race/skip line: focused 89 PASS / 0 FAIL / 2 UID0-only SKIP, final `ok` 13.544s (`task-2-final-focused.log:187`); host package race 428 PASS / 0 FAIL / 2 UID0-only SKIP, final `ok` 28.854s (`task-2-final-race.log:866`). Expected skips name actual chown cases; no final warning/panic/data-race markers found. Counts include aggregate parent subtests as the report states.
- Inspected own compiling RED result summaries: protected 0/3/1, gate 0/5/0, persistence 18/5/0, recovery 5/11/0, command 0/2/0 PASS/FAIL/SKIP. Full missing-release mutation log records expected failure after 5 seconds with child killed (`task-2-fatal-cleanup-red.log:3`); restored command/Fatal run has 14 PASS / 0 FAIL / 0 SKIP (`task-2-command-fatal-green.log:30`). This supports the existing join regression but does not answer I1.
- Verified saved final vet, gofmt, diff-check, Windows unsupported build and Linux CGO0 build logs exist and are empty. Their exit-0 claims are supplied by the complete implementation report; empty files alone are not independent exit-code evidence. Both cross-builds are compilation only. The report retains its failed test-helper type assertion attempt and corrected full focused run (`task-2-report.md:66`, `:80`); no such failure appears in final output.
- No tests, race detector, vet, cross-build or Docker commands were rerun. No focused experiment was necessary: I1 follows directly from the child/parent control flow and existing mutation evidence covers a different condition. No changed source file was redundantly reread.

# Review scope, full-read metrics and evidence limits

- Reviewed frozen source package BASE `64632d67888942300dba6be509e8bce21056acec`, HEAD `f07450c9ff7ca9e21277e9c61d88b32411314b3b`. The supplied package lists four product commits, seven new files and 1995 added source/test lines. Read all 63243 bytes / 2056 lines in consecutive intervals 1–340, 341–680, 681–1020, 1021–1360, 1361–1700, 1701–2056; no interval was truncated, no hunk required a separate source-file read. A subsequent diff-derived line-number index was used only to attach exact references.
- Read task-reviewer method in full; brief first among task artifacts; binding review context 12 lines / 2523 bytes; formal context 145 lines / 24931 bytes; implementation report all 120 lines / 21517 bytes in three bounded intervals. Read all requested Root Linux evidence. A first wc command mistakenly appended .md to the log/JSON filenames; this was a read-only nonexistent-path error and corrected immediately by reading their exact stated paths. No missing evidence was substituted with a rerun.
- Named concrete risks checked within the diff: FIFO blocking; canonical path/pinned directory substitution; owner/mode/link validation; flock lifetime; complete fsync chain; possible committed bytes; poison/cancellation/false receipts; missing-manifest reconstruction; exact recovery binding; paged memory use and capacity; nil/zero/closed mutex behavior; private point primitive contract; Fatal worker release/join and its parent-child result contract; unsupported-platform complement. No unchanged-code checks were needed: Task 1 API/codec guarantees are explicitly provided in the full binding context and remain outside this diff. No broader codebase/history/sibling crawl or git commands were performed.
- ⚠️ Source-to-binary provenance and frozen-HEAD association are controller/report evidence, not independently reconstructed by this reviewer. Root's JSON supplies binary hash and execution identity; retain its build/freeze provenance. No need to rerun accepted verification on unchanged code merely for this review.
- ⚠️ The report's artifact-copy incident and deletion of 15 unrelated accidental destination copies are disclosed (`task-2-report.md:114`–`:120`). This reviewer did not access the unrelated originals, credentials, configs, ignored user data or Sentinel content. Controller owns any further process/audit check of that cleanup; those copies supply no evidence for this verdict.
- ⚠️ Actual power-loss/host-failure durability, OS-fsync immediate cancellation, UID/PID1 isolation and deployment identity protection cannot be established by local process-restart tests. The report correctly limits those claims (`task-2-report.md:108`). Original Task 1 strict schema/encoding internals were not re-reviewed; only their supplied interfaces and Task 2 use were checked. Full cross-task integration remains a controller gate.
- Source/index/HEAD/branch remained read-only. Only this owned review report was created. No subagents, worker reviewer or external messages/actions were used.

# Assessment

**Task quality: Needs fixes.** Counts: **0 Critical, 1 Important, 1 Minor**. The production file protection, persistence ordering, recovery/accounting and lifecycle implementation are consistent with the scoped requirements and supported by substantial real-IO evidence. Correct the falsely passing Fatal-cleanup postcondition gate before approving Task 2; retain later Linux/full-package, Task 3 and whole-migration gates.

## Complete saved task-2-fix-1-review.md

**I1 — Fatal-cleanup regression swallows failed postconditions — ADDRESSED.** `internal/runtime/controltarget/journal_gate_test.go:434` initializes an explicit cleanup result independently of the intentionally failed `testing.T`; `:435` makes each cleanup diagnostic also clear that result. Closing the original journal (`:436`), observing poison (`:439`), reading retained evidence (`:442`), finding retained evidence (`:450`), reopening to check lock release/retained bytes (`:453`), and closing the reopened handle (`:456`) all participate. The accepted marker is now emitted only when that complete result remains true (`:459`). Intentional Fatal alone does not suppress success, while an additional failed required postcondition does.

**I1 negative verification — ADDRESSED.** The parent now runs both complete and missing-retained-evidence subprocess cases (`internal/runtime/controltarget/journal_gate_test.go:375`). Both must exit 1 with the intentional Fatal diagnostic (`:390`); marker presence must match the case expectation (`:393`), and the negative case additionally requires both the real mutation marker and the retained-evidence failure diagnostic (`:397`). The negative child removes actual retained `.gate.*` files from its owned journal only after the registered worker cleanup (`:413`, `:421`), refuses to claim mutation if none was removed (`:427`), and records successful mutation (`:430`). This meets the requested non-join negative experiment and cannot be accepted solely because an arbitrary child failure happened. The unchanged worker release/cancel/join order is outside the fix hunk's modifications; the fix retains synchronous CombinedOutput, the five-second context and one-second WaitDelay (`:383`, `:386`, `:388`).

## New breakage in the fix diff

**None.** Inspected the complete single-commit fix package `review-f07450c..7fcbd99.diff` once: 5,486 bytes, one test file, 61 insertions and 17 deletions. The changes preserve the child process failure contract while distinguishing actual cleanup success, operate only on the owned negative-test fixture, and also require the reopened handle's Close result. No product/API/platform changes appear. New finding counts: **0 Critical, 0 Important, 0 Minor**.

## Out-of-scope observations

**Existing M1 remains carried forward, non-blocking for this fix round.** The previous review's hard-exit fixture lacks retained unknown-record bytes. Root explicitly ledgers that acceptance for Task 3 using actual public RecordUnknown and real subprocess restart; the implementation report expressly preserves that deferral. This review neither closes M1 nor expands this I1-only loop. No additional out-of-scope issue was identified.

## Saved evidence inspected

- **Compiling behavioral RED confirmed:** read full `task-2-i1-red.log`. The exact reported selector was `go test ./internal/runtime/controltarget -run '^TestJournalPersistenceFaultsWorker$/fatal-cleanup-regression$/missing-retained-evidence$' -count=1 -v`, reported actual exit 1. Its output includes `cleanup success marker=true want=false`, `MUTATED_RETAINED_EVIDENCE_REMOVED`, the original unconditional success marker, the intentional Fatal diagnostic and `uncertain evidence removed`. Three outer FAIL nodes and the captured child FAIL demonstrate the specific formerly false acceptance, rather than a setup/compiler error.
- **Focused GREEN confirmed:** read full `task-2-i1-green.log`; the report names `go test ./internal/runtime/controltarget -run '^TestJournal(PersistenceFaultsWorker|FatalCleanupHelper)$' -count=1 -v`, actual exit 0. Output shows both subprocess branches passing, two top-level/six inclusive PASS nodes, 0 FAIL, 0 SKIP, final package `ok` in 0.586s.
- **Host race confirmed separately:** read full `task-2-i1-race.log`; the report names the same selector with `-race`, actual exit 0. Output shows two top-level/six inclusive PASS nodes, 0 FAIL, 0 SKIP, no race warnings and final package `ok` in 1.610s. This is host race evidence, not Linux race.
- **Root native gate confirmed:** read the complete `task-2-fix-1-root-linux-report.md`, all 79 lines/5,307 bytes of `task-2-fix-1-root-linux.log`, and complete `task-2-fix-1-root-linux.json`. The run selects `^TestJournal(ProtectedFiles|PersistenceFaultsWorker)` and has four top-level/39 inclusive PASS nodes, 0 FAIL, 0 SKIP, no warning/panic output. Both complete and missing-retained-evidence subprocess branches are present alongside real ownership/CHOWN tests. JSON records attach exit 0, inspected actual test ExitCode 0, wait result 0 and fixture/test exit 0.
- **Root resource isolation/cleanup evidence confirmed:** JSON records actual image architecture arm64, UID0, network none, drop ALL plus CHOWN only, no-new-privileges, readonly binary bind and owned /tmp volume. Exact owned container and volume removal each exit 0; the final matching container/volume/network inventories are empty with exit 0. The image is the bound pinned `gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1`.
- **Commands/exits/provenance report checked:** read all fix append lines 121–156 of `task-2-report.md`, including the final supplied Root result. It names actual exit-0 vet, gofmt, scoped diff-check and Linux CGO0 arm64 test-binary compile commands and distinguishes compile from native execution. These ancillary empty-log claims were not independently re-executed or re-read; no code-reading doubt required them to resolve I1. The supplied freeze is fix base `f07450c9ff7ca9e21277e9c61d88b32411314b3b` to HEAD `7fcbd994fcb40e70d6ba144ea8bd4a0accf53a2d`; Root states actual HEAD/ELF machine 183 verification and JSON records new binary SHA256 `c50e958056b878a5066fd565666afd7ee5110bcc027b4495dd0a4b11ec6de8b3`.

## Review scope and evidence limits

- **Method/scope check completed:** read the complete scoped re-review prompt; task brief; binding review context; full Task 2 formal context in bounded intervals 1–72 and 73–145; previous review including verbatim I1 and M1 disposition; complete fix report append; complete supplied fix diff; and the evidence enumerated above. No output interval was truncated. The original 63KB Task 2 package and unchanged product code were not re-reviewed.
- **Evidence limitation retained:** native source-to-binary provenance and actual frozen HEAD are Root/report evidence; this reviewer did not reconstruct the binary or inspect Git state. The JSON corroborates the supplied binary path/hash and actual execution/cleanup. Successful subprocess output is intentionally captured by the parent and not printed in GREEN/native logs, so the changed parent oracle plus the explicit RED log supply the detailed child result-contract check.
- **Acceptance limits retained:** this fix verifies a test oracle and real process cleanup; it does not establish power-loss durability, Linux race/full-package acceptance, Task 3 unknown persistence/restart acceptance, actual target/PID1 authority, launch/drain, deployment isolation, physical quotas, GC, production wiring, or whole-migration completion. Existing later gates remain required.
- **Read-only boundary observed:** no test/race/vet/build/Docker reruns, Git commands, source/index/HEAD/branch mutations, subagents, broad crawl, unrelated workspace/configuration/credential/ignored-content/Sentinel reads, or external messages. Only this owned report file was created.

## Verdict

**Fix round 1: All findings in scope addressed, no new Critical/Important breakage.** I1 is **ADDRESSED**; scoped open Critical/Important findings: **0**. Existing non-blocking M1 remains explicitly ledgered for Task 3 and is not silently discarded. This is scoped Task 2 fix approval, not whole-branch or migration approval.

## Complete saved task-2-root-linux-report.md

# Task2 Root actual Linux ownership gate

Frozen source f07450c9ff7ca9e21277e9c61d88b32411314b3b. Worker actual Linux CGO0 build, actual pinned image architecture arm64 independently inspected. ELF64 little-endian machine183 verified before creating fixture; binary SHA256 cd8666c1186dd4f968a7e5711a2a9908fed54b198c01fecaa6e3102b67b20e4e. Native only, not Linux race.

Root command: python3 .superpowers/sdd/2026-10-07-runtime-target-journal/root-linux-fixture.py .superpowers/sdd/2026-10-07-runtime-target-journal/task-2-linux-arm64.test '^TestJournalProtectedFiles' --tag task-2. Toolactualexit0; docker attach/inspect/wait confirm actual test exit0. Complete log 4529bytes/69lines FULL read. 34PASS nodes (3top-level),0FAIL0SKIP; no warnings/panic/race markers. Actualwrong-owner root/commands/bucket/gate/lock/record and pinned-root substitution run, not mocked or hostskip.

Project sandbox-target-journal-test-63970-1791320212192913000; fresh exact labels, networknone, UID0, dropALL+CHOWN only (no DAC_OVERRIDE), NNP, ownbinaryread-onlybind, ownvolume/tmp; no etcd process. No shell/network or hostwritablemount/credentials used. Exact owned container+volume removal each actualexit0; label inventories containers/volumes/networks all0. Full provenance in task-2-root-linux.json and raw output task-2-root-linux.log. No resource remains.

This gate verifies native ProtectedFiles selector only; host race and remaining Task2 selector evidence separately worker-reported. Does not certify powerloss/host failure, physical sandbox PID1/isolation or overall Redis migration.

## Complete saved task-2-fix-1-root-linux-report.md

# Root Task2 fix1 actual native gate
FrozenHEAD7fcbd994fcb40e70d6ba144ea8bd4a0accf53a2d; onlyjournal_gate_test.go changed, product APIs unchanged. New worker CGO0 arm64 binary explicitunique path, original preserved; RootactualHEADcheck and ELFmachine183 verify, SHA256c50e958056b878a5066fd565666afd7ee5110bcc027b4495dd0a4b11ec6de8b3. Command root-linux-fixture.py task-2-fix-1-linux-arm64.test '^TestJournal(ProtectedFiles|PersistenceFaultsWorker)' --tag task-2-fix-1. actualexec/testexit0; attach+inspect+wait confirms0, 39PASS(4top),0FAIL0SKIP, no warnings/panic. Fullrawlog FULLread 5307bytes 79lines. Includes real Fatal success branch and missing-retained-evidence negative child branch plus actualRootownerCHOWN/noDACoverride cases; intentionalchildexit1 consumed by parentnot a hidden overall failure. Hostfocus/race separate worker report.
Projectsandbox-target-journal-test-65959-1791321154461169000 exactlabels, networknone/UID0/dropALL+CHOWN/NNP/binaryRO/test-owned/tmpvolume; no shell/etcd/hostwritablecredentials. Exactownedcontainer+volume removals exit0, final labelinventories containers/volumes/networks0 (fullJSONsaved). All resources removed. No Linuxrace/fullpackage/powerloss/physicalauthority claim.

## Complete saved task-3-report.md

# Task 3 implementation report

Status: DONE. Source frozen and Root-owned Linux UID0 native script execution passed; independent Spec/Quality review and overall migration follow-up remain Root-owned.

Workspace: `/Users/dysodeng/project/go/cloud/sandbox`; branch `codex/etcd-state-management`; exact starting HEAD `116fca7d85c98fa4b5c2539de3bf9c341b21e5b0`; frozen source HEAD `4dd09c8`.

Read task-3-brief.md first, then complete task-3-context.md, implementer-prompt.md, TDD and verification skills/references. No whole plan/history/sibling reads, no subagents, no Docker lifecycle calls by worker. Pre-existing modified Sentinel document was left unchanged and unstaged.

## Commits and files

- `73018ad feat(runtime): persist immutable diagnostic execution evidence`
  - New `internal/runtime/controltarget/journal_exec.go`, `journal_exec_test.go`.
  - `journal_accounting.go`: only relocates unchanged private `recordBinding` to the common new file. This preserves its exact identity/runtime/epoch matching behavior and ensures public API compilation on unsupported platforms.
- `4dd09c8 test(runtime): verify diagnostic intent persistence and fixed point IO`
  - New `journal_exec_fault_test.go`, `journal_capacity_test.go`, executable `scripts/test-runtime-target-journal.sh`.
  - `journal.go`: adds synchronous private `journalFiles.observe func(operation string)`; cannot inject failure or replace IO; set only with no operation in flight.
  - `journal_files_unix.go`: observes actual point syscall sites and raw unix.Read/unix.Write attempts, including EINTR retries/EOF. Existing dirFD/no-follow/owner/mode/link/bounded-read/fsync/atomic-rename/accounting/check/poison behavior is retained.

Exact base-to-freeze delta: 8 files, 1228 insertions, 20 deletions. No controlprotocol, etcd, Manager, config, legacy runtime, storage, image or dependency changes. No clock/provider/launcher/auth authority added; no new product goroutines, timers, history scan or cache.

## Public behavior

`RecordUnknown` checks nil context/handle, takes journal mutex, checks write availability (including poison), consumes only opaque `ExecStartEvidence`, verifies bounded canonical input wire SHA256/digest, strict nested schema and complete getter consistency, then binds the entire context to the current journal. It does not reverify crypto or current freshness: authenticity came from the existing opaque producer; consistency never confers authority. Tickets/signatures/payload/argv/env/stdin/stdout are never persisted.

The controlprotocol canonical input uses json.Marshal (HTML escaping), while persisted journal records retain the Task1 SetEscapeHTML(false) codec. Complete canonical record bytes determine same-ID idempotence; any full context, digest or window difference conflicts. The existing retry read occurs before projected85% and content-file cap checks. The private persist helper retains its second absence point read and never overwrites an existing command. A failed write returns no success record but may retain a full committed unknown.

`Lookup` takes the same mutex and performs a strict real disk point read, including on poisoned handles. Missing bucket/file gives nil,nil; corrupt/oversize/unsafe/binding-invalid files return errors. Returned record/context/digest/time scalars are owned. Closed handles return ErrClosed; nil/zero unavailable; nil ctx invalid configuration. Close/Status/Lookup contention is exercised in the new lifecycle test.

Original Task2 private contracts remain: `checkLocked`, `readCommandLocked`, `persistNewCommandLocked`, `recordBinding`, `poison`, `journalIOHook` and `newJournal` signatures unchanged. `recordBinding` location alone changed. Added observer is synchronous and nil in public constructors. Observation is only at point IO sites; constructors/cold scans are deliberately excluded from the hot-path measurement. Directory-page detection uses existing real Readdirnames128 hook and reports zero hot-path calls.

## TDD and every encountered failure

All log paths below are explicit files under `.superpowers/sdd/2026-10-07-runtime-target-journal/`. No /tmp glob or unrelated ignored-file access occurred.

1. `go test ./internal/runtime/controltarget -run 'TestJournal(RecordUnknown|ExecHandles|Lookup)' -count=1 -v` — exit1, `task-3-red.log`. 6 top-level failures /22 total failure result rows. API tests behaviorally failed with `Journal has no diagnostic RecordUnknown/Lookup API`. The independent ExecHandles fixture also exposed missing ExecutionRequest UID/GID (`execution uid and gid must each be 1..2147483647`). This helper setup error is not claimed as behavior RED.
2. Corrected helper UID/GID to1000, unsafe-uppercase fixture UUID to one containing letters; reran identical command — exit1, `task-3-red-corrected.log`. 6 top-level/22 total failures, all the intended missing API assertion; this is actual behavioral RED before production implementation.
3. First implementation ran identical focus — exit1, `task-3-green.log`, 5 top-level/20 total failures, 2 total passes. Authentic evidence with authority `<&>` exposed real protocol canonical HTML escaping mismatch (`noncanonical execution evidence`). Corrected consistency encoding to existing controlprotocol json.Marshal; retained journal's own canonical codec. No source change to controlprotocol.
4. Reran focus — exit0, `task-3-green-corrected.log`, 6 top-level/34 total PASS, 0FAIL0SKIP. Record1095B, input ticket1156B, gate361B.
5. Fixed-point test written before observer production helper: `go test ./internal/runtime/controltarget -run '^TestJournalFixedPointCost$' -count=1 -v` — exit1, `task-3-observer-red.log`, compile failure `j.files.observe undefined`. This is explicitly a missing observation-helper build RED, not a behavioral API assertion RED.
6. After syscall-site observer implementation, same fixed-point command — exit1, `task-3-observer-first-green.log`: actual raw read attempts4 versus mistaken expectation2 for both0/1000 fixtures. Actual new-write counters already matched. Corrected test oracle to observed bounded ReadAll512/384/199B +EOF reads, with explicit raw syscall counts. This oracle/setup failure is not claimed as product behavior RED.
7. Capacity/fixed-point GREEN command below exit0. No other helper/build/test errors occurred.

## Actual verification commands and results

Shell redirects captured complete outputs to the named logs; wrapper saved `$?` and exited that exact result. Package tests are fresh `-count=1`, not cached.

| Command | Exit / output | Log |
| --- | --- | --- |
| `go test ./internal/runtime/controltarget -count=1 -v` (API slice) | 0; package15.288s; 30 top-level PASS,464 total PASS,2 expected host root-only SKIP,0FAIL | task-3-slice1-package.log |
| `go test -race ./internal/runtime/controltarget -run 'TestJournal(RecordUnknown\|ExecHandles\|Lookup)' -count=1` | 0; package2.699s; PASS | task-3-slice1-race.log |
| `go vet ./internal/runtime/controltarget` (API slice) | 0; no output | command output retained in tool execution |
| `go test ./internal/runtime/controltarget -run 'TestJournalExec(PersistenceFaults\|RestartRetainedUnknown\|ConcurrentLifecycle\|Fatal)' -count=1 -v` | 0;1.801s;5 top-level/23 total PASS,0FAIL0SKIP | task-3-faults.log |
| `go test ./internal/runtime/controltarget -run 'TestJournal(Capacity\|FixedPointCost)' -count=1 -v` | 0;1.171s;3 top-level/8 total PASS,0FAIL0SKIP | task-3-capacity-green.log |
| `go test ./internal/runtime/controltarget -count=1 -v` (complete Task3 source) | 0;18.389s;38 top-level/495 total PASS,2 expected host root-only SKIP,0FAIL | task-3-slice2-package.log |
| `go test -race ./internal/runtime/controltarget -run 'TestJournal(Exec\|Capacity\|FixedPointCost\|RecordUnknown\|Lookup)' -count=1 -v` | 0;6.265s;14 top-level/65 total PASS,0FAIL0SKIP | task-3-slice2-race.log |
| `go vet ./internal/runtime/controltarget` (complete source) | 0; no output | task-3-vet.log |
| `bash -n scripts/test-runtime-target-journal.sh` | 0; no output; repeated after final cleanup-attempt ordering refinement | tool command output |
| `gofmt -w` new/affected Go files, then `gofmt -l` exact seven Go paths | 0; final list empty | tool command output |
| `git diff --check` before each commit; `git diff 116fca7d85c98fa4b5c2539de3bf9c341b21e5b0 HEAD --check` after freeze | 0; no whitespace errors | tool command output |

Top-level and total result rows overlap by design; counts do not sum distinct test invocations across repeated suites. Expected host-only skips are exactly `TestJournalProtectedFiles/actual-wrong-owner` and `TestJournalProtectedFilesLayout/actual-layout-wrong-owner` because macOS host test UID is nonroot. They do not prove actual chown/root isolation. Native Linux Root execution is required separately. No other warnings occurred; intentionally failing Fatal children are validated by the parent tests and are not package failures.

Full suite consumes the existing actual Task2 `TestJournalPagedAccountingFileLimit` (65535 content-file reserve-slot/65536 rejection) and `TestJournalPagedAccounting` (1000-history/13 real Readdirnames128 page requests) coverage, rather than duplicating a second65534-file fixture. This public consumer's same-ID-before-new-cap ordering is additionally verified using byte-soft/hard-cap fixtures.

## Actual persistence, bounded concurrency, M1 evidence

16 actual synchronous fault cases: write/file-sync/rename/dir-sync × before/after × injected error/ctx cancellation. Warm bucket22 isolates command write failures from creation fsyncs. Every attempted uncertain write poisons, sets AccountingKnown=false/NewRecordsStopped=true, preserves the last verified counters and every retained byte, rejects subsequent RecordUnknown and CloseGate. Lookup still strictly checks disk. Before-write faults leave empty temp and cold reopen rejects it; other faults retain complete valid temp or committed unknown and cold reopen recounts it. After rename and every dir-sync failure/cancel may coexist with full committed command. No false successful result.

M1: `TestJournalExecRestartRetainedUnknown` calls real public RecordUnknown with genuine verifier-produced opaque evidence, captures the complete returned record, full command bytes, full gate bytes and complete JournalStatus. It then closes parent, uses existing actual child opener which `os.Exit(23)` hard exits without Close, and cold reopens another handle. Exact1095B unknown, exact361B closed gate, LogicalBytes1456, Records1, TemporaryFiles0, AccountingKnown=true and all Context/digests/window match the pre-exit snapshot. Idempotent retry still returns the same full record; a separately authenticated extended start window conflicts. No replay or capability is created. Exit23 and actual flock release are checked by the existing bounded child helper.

New blockUnknownSync helper registers cancel/release/<=5s join cleanup before a caller can Fatal. It blocks only after actual file fsync; raw bytes are complete. Hook restoration is strictly after join, and fixture/FD cleanup afterward. New concurrent test also joins its Lookup/Close/Status workers before hook restoration. New Fatal child regression has independent cleanupOK oracle despite t.Failed already being true. Positive case must emit `UNKNOWN_WORKER_JOINED_AND_RETAINED`; negative case actually removes only its own retained unknown temp after join and must emit `NEGATIVE_UNKNOWN_REMOVED`, report `retained unknown temp missing`, and omit the success marker. Both child tests intentionally exit1, and parent package tests verify those exact outcomes. Product contains no worker/goroutine; test workers are the only asynchronous additions.

## Capacity and syscall measurements

Actual64KiB public authentic fill:50 ×1095B unknown +361B gate =55111B. Warning true; projected51st record56206B >=85% is denied without mutation. Original retry/query and durable CloseGate still succeed. No unknown is deleted.

Actual valid bounded unknown files (strict-schema legal whitespace padding is counted, each <=8192B) cold-open at55706B and65175B with8/9 records. Existing authentic record retry/query and CloseGate succeed above85%; new distinct command is denied. At65175B the361B transient gate reaches exactly65536B hard budget. At actual65536B, cold durable close would exceed hard budget and Open fails ErrCapacity preserving all bytes and the original authentic unknown. No promise that cleanup can always succeed at hard limits.

Counts reset **after** cold-open scan and durable closed gate. Both histories0 and1000 use an existing/warm bucket22 (same layout); synthetic history uses real disk files. New command id `22ffffff-ffff-4fff-8fff-ffffffffffff`. No cold scan or first-bucket setup is included.

| Operation | openat | fstat | raw read attempts | raw write attempts | file fsync | renameat | directory fsync | close | directory-page/scan |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| New RecordUnknown | 6 | 4 | 0 | 1 | 1 | 1 | 1 | 4 | 0 |
| Same canonical retry | 2 | 3 | 4 | 0 | 0 | 0 | 0 | 2 | 0 |
| Lookup existing | 2 | 3 | 4 | 0 | 0 | 0 | 0 | 2 | 0 |
| Lookup absent in warm bucket | 2 | 1 | 0 | 0 | 0 | 0 | 0 | 1 | 0 |

Counts are actual instrumented synchronous syscall-site attempts on the host fixture, including failed opens and EOF read; syscall errors/EINTR remain possible, so this is an observation rather than a universal syscall upper bound. Both0/1000 results matched each table entry and each other. New write counts include public first absence point read and private second absence point read. An unwarmed bucket would add actual directory creation/fsync-chain work; it is explicitly excluded here. Query reads1095B through bounded ReadAll, which requests512/384/199B and EOF, not an invented single read. All persisted observed new records1095B.

No actual large-N Pod test, whole-system constant recovery, QPS, p99, power-loss, daemon isolation or production PID1 claim is made. Cold scan grows with each runtime's own history; directory/block/FD overhead remains separate from LogicalBytes. Existing actual filesystem metadata overhead logging is in consumed Task2 package tests. Runtime process/FD/disk/GC budgeting and complete later migration phases remain mandatory.

## Linux native script handoff / source freeze

Root notified at HEAD4dd09c8; all product/Go/script source frozen until Root actual execution and independent review. Worker wrote/syntaxchecked script but did not inspect/create/start/remove/pull Docker resources. Root request explicitly includes running the exact script, log/counts/actualexit and exact cleanup verification before DONE.

Script inspects **existing exact pinned image** for Linux architecture arm64/amd64, builds `CGO_ENABLED=0 GOOS=linux GOARCH=<inspected> go test -c`, uses readonly test-binary bind and test-owned /tmp volume, UID0, networknone, read-only container root, dropALL+CHOWN, no-new-privileges, no credentials/host writable mount, and does not run etcd/pull images. Name prefix `sandbox-target-journal-test-<pid>-<unix>`; both volume/container have exact project and fixture labels. Creation attempts are marked before Docker call so response loss can still be cleaned up, but cleanup requires exact label match. EXIT/INT/TERM trap deletes only exact owned resources and own mktemp scratch; saves incoming exit. Attach result and actual container State.ExitCode are printed and actual test exit is preserved. Native output includes FAIL/SKIP counts; Linux native is not Linux race and compile is not execution.

The requested Root script evidence is appended below. Host focused race and Linux native remain separate. Root owns final host full package/race, full repository test/vet/build, full new delta and affected integration review. Overall phases1–5, authenticated target, actual PID1/descendant drain, scheduler/tasks/collector/GC/native wiring and complete Redis removal remain required after this intermediate journal unit.

## Self-review

Read own complete production diff and new test/script sources before each coherent commit, checked canonical records include every scalar field/window, poison checks precede retries, unknown diagnostics remain strict, no input payload persists, and fixed point tests measure both absence reads. Initial helper UID/GID, HTML-escape discrepancy, actual read4 oracle correction and uncertain Docker-create cleanup were corrected and all actual failures retained above. Script cleanup attempt booleans were moved before create commands during self-review, syntaxchecked again, then committed. Existing user Sentinel edit was excluded.

No unresolved product correctness concern identified by self-review. Independent Spec/Quality review is Root-owned. Native Linux execution evidence was supplied by Root and inspected at the exact named artifacts before returning DONE.

## Root actual Linux UID0 execution at frozen source

Root executed `bash -n scripts/test-runtime-target-journal.sh && bash scripts/test-runtime-target-journal.sh` at exact `4dd09c8f0c539a5729972ad498a6445a1b4161fa`. Worker inspected only the explicitly provided own-task `task-3-root-linux-report.md`, `task-3-root-linux.json`, and the header/tail of `task-3-root-linux.log` after Root's result message. Full raw Linux output is retained in that log by Root.

Actual image-inspected architecture:arm64 on the exact pinned image specified above. Actual result:503 total PASS rows,38 top-level PASS,0FAIL,0SKIP. Container actual test exit0, Docker attach exit0, canonical script wrapper exit0. Ownership/chown tests that skip on nonroot host executed in this Linux UID0 fixture. Native CGO0 remains distinct from host race; this does not prove power-loss/host-fault/production-isolation behavior.

Exact owned project: `sandbox-target-journal-test-68272-1791322082`. Independent post-trap project-label container, volume and network inventories each returned exit0, empty remaining lists, and empty stderr. Script trap ran exact-owned cleanup, but individual Docker rm exit codes were suppressed by the script and were not logged; no claim is made about individual rm exit evidence. The independent zero inventories confirm no owned Docker resource remains.

No product/source/HEAD changes occurred after freeze. Final `git status --short` showed only the pre-existing Sentinel document modification; final script syntaxcheck exit0. All requested worker deliverables are complete; independent review and final integration gates remain controller-owned.

## Complete saved task-3-review.md

### Spec Compliance

- ✅ Spec compliant for Task 3's diagnostic producer/query, immutable persistence, fault/restart acceptance, capacity/point-cost tests and Linux fixture deliverable. Reviewed the complete two-commit delta `116fca7d85c98fa4b5c2539de3bf9c341b21e5b0..4dd09c8f0c539a5729972ad498a6445a1b4161fa`; all five requested new files are present. The three private-helper edits are narrowly related to shared binding and actual syscall observation.
- ✅ `journal_exec.go:27` converts verifier-produced opaque evidence into only Context, digests and window; validates bounded canonical wire, SHA256 and getter consistency; persists no ticket/signature/payload. `journal_exec.go:67` checks write availability, complete journal binding and full canonical equality before the new-record helper. It performs no freshness verification, Clock/provider call or authority reconstruction. `journal_exec.go:114` exposes a mutex-protected owned point query.
- ✅ `journal_exec_test.go:37` obtains genuine opaque evidence through existing signing and management verification, and verifies damaged signatures cannot produce nonzero evidence. The tests cover zero/invalid handles, historical evidence, all binding dimensions, full context/window conflicts, input/result mutation, absent/corrupt/oversize and unsafe disk files.
- ✅ `journal_exec_fault_test.go:22` exercises write/file-sync/rename/dir-sync before/after error/cancellation against real persistence; checks poison, unknown accounting, no false successful record, strict Lookup and retained committed/temp bytes across cold reopen.
- ✅ Task 2 carried M1 is closed: `journal_exec_fault_test.go:149` invokes the real hard-exit child after public RecordUnknown; complete record, command bytes, gate bytes and complete Status are compared after reopening, with immutable retry and extended-window conflict. The unchanged child actually opens the journal and calls `os.Exit(23)` without Close (`journal_gate_test.go:182`). The recorded result is exact unknown1095B + gate361B = LogicalBytes1456, Records1, TemporaryFiles0, closed/known accounting (`task-3-root-linux.log:781`). No replay API or launch capability was added.
- ✅ `journal_capacity_test.go:18` fills an actual64KiB journal with authentic records, observes70% warning and projected85% denial while preserving retry/query/CloseGate. `journal_capacity_test.go:167` records actual valid55706B/65175B boundary fixtures; the65536B case requires failed cold durable close and retained bytes. `journal_capacity_test.go:231` compares observed point syscalls for real0/1000 disk histories after cold setup, including the private second absence read and zero directory paging. No large-N/QPS/p99 claim is made.
- ✅ `scripts/test-runtime-target-journal.sh:6` pins the required existing image; image inspection determines architecture; the fixture uses CGO0 `go test -c`, readonly binary/root, owned volume, UID0, networknone and dropALL+CHOWN. Exact project/fixture labels govern trap cleanup. Root's frozen-source run has503 PASS rows/38 top-level,0FAIL0SKIP, actual/attach/script exits0 and independent zero owned-resource inventories (`task-3-root-linux-report.md:3`, `task-3-root-linux.json:2`).
- ⚠️ Cannot verify from this task diff: the full pre-existing constructor/ownership/flock/paged accounting implementation was not re-audited; it is inherited from the accepted Task2 gate and full package evidence. Final controller host full-package race, repository test/vet/build, full-unit integration review, authenticated target/PID1/drain/tasks/scheduler/collector/GC/native wiring and complete Redis removal remain controller-owned subsequent gates. This approval does not establish those later migration requirements.

### Strengths

- `journal_exec.go:96` canonicalizes the complete saved and proposed records before comparison, so matching IDs/digests cannot silently change a start window. Existing retries precede new-record capacity checks; the underlying helper independently checks absence and updates counters only after successful persistence (`journal_files_unix.go:420`).
- `journal_exec.go:126` preserves exact journal identity/runtime/gate-epoch matching; `journal_files_unix.go:384` validates fixed filename ID and decoded binding rather than trusting cached history. Diagnostic reads remain possible on poisoned handles.
- `journal_files_unix.go:98` observes raw read attempts, including EINTR/EOF; `journal_files_unix.go:305` observes actual mutation syscall sites rather than replacing storage with a mock. The measured counts explicitly exclude cold paging and bucket creation, and include real record bytes (`journal_capacity_test.go:231`).
- `journal_exec_fault_test.go:185` installs cancellation/release/bounded join before callers can Fatal and restores hooks after join. `journal_exec_fault_test.go:299` uses an independent cleanup oracle and a negative retained-evidence deletion case, preventing the intentionally failed child's pre-existing failure flag from manufacturing success.
- Stored final host output has495 PASS/0FAIL/2 explicit nonroot ownership SKIPs; focused race has65 PASS/0FAIL0SKIP; fault and capacity evidence has23 and8 PASS respectively. Linux UID0 actually executes the ownership cases with0SKIP. The implementation report preserves initial helper/oracle/canonical-escaping failures and correctly separates native CGO0 from host race.

### Issues

#### Critical (Must Fix)

- None found.

#### Important (Should Fix)

- None found.

#### Minor (Nice to Have)

- T3-M1 — `scripts/test-runtime-target-journal.sh:80`: the script reads `.State.ExitCode` without independently checking a terminal container state before printing it as `actual_exit`. Created/running containers also expose a default zero ExitCode, so the evidence currently relies on successful `docker start --attach` having waited for completion. The existing branch at line89 correctly rejects nonzero attach results even when ExitCode is zero, so ordinary attach/daemon errors do not produce a false success; no false pass was observed in this run, whose final PASS and cleanup are recorded. For clearer repeatable evidence, inspect `.State.Status`/`.State.Running` and require exited before treating ExitCode as a completed test result, or obtain the result from an explicit container wait. This is a robustness improvement, not a Task3 gate blocker.

### Assessment

**Task quality:** Approved

**Reasoning:** The public API preserves the passive diagnostic boundary, authentic immutable records, poison semantics and bounded point operations while reusing the actual protected filesystem implementation. Real fault, hard-exit, capacity, race and Linux UID0 evidence supports this task's acceptance; the optional terminal-state assertion would make script completion evidence more explicit.

### Review checks and scope

- Read brief and binding global constraints first, complete actual contracts, full implementer report and Root Linux report; read the full55949-byte diff once in three contiguous portions. No git commands, suites, Docker lifecycle operations or subagents were run.
- Named risk — authentic evidence ownership/canonical contract: checked unchanged `controlprotocol/exec_start.go:57` and `exec_start_types.go:41`; evidence fields are private, Wire returns a copy, verifier normalizes the envelope after signature/window validation, and4096 is the actual protocol wire bound. A guessed `types.go` path was absent; the focused symbol search located `exec_start_types.go`. No evidence gap remains.
- Named risk — moved common binding/new public methods on unsupported builds: checked unchanged `journal_files_unsupported.go:7`; constructors reject configuration and private helper stubs remain defined. No cross-compilation or unsupported-platform execution claim is made.
- Diff cutoff exception — the readCommandLocked/persistNewCommandLocked hunks ended mid-function. Read only `journal_files_unix.go:376..470` to check filename/binding validation, second absence lookup, reserve-slot/85% checks, poison propagation and post-success accounting. No other changed-file reread was performed.
- Named risk — hard-exit restart oracle: checked only unchanged `journal_gate_test.go:162..201`; child execution is bounded, expected exit23 is checked, and the opener exits without Close.
- Named risk — script completion after attach/daemon failure: inspected the already-read script's result branches; nonzero attach/default-zero ExitCode is rejected, terminal state is not independently asserted. Assessment is Minor T3-M1 above.
- Reviewed the explicitly named stored logs rather than regenerating them. Counted result rows in final Linux, host package, focused race, fault and capacity logs; counts match the reports. Checked named capacity/point-cost/M1 output and Linux actual exit lines; inspected Root's explicit cleanup JSON. Expected host root-only skips are disclosed; no unexplained final FAIL or warning was found. No new test run was necessary to resolve a concrete doubt.

## Complete saved task-3-root-linux-report.md

# Root Task3 Linux native verification

Exact frozen source4dd09c8f0c539a5729972ad498a6445a1b4161fa; actual `bash -n scripts/test-runtime-target-journal.sh && bash scripts/test-runtime-target-journal.sh` succeeded. Shell wrapper captured actual script exit0; test actual_exit0 and attach_exit0 printed. Complete raw log `task-3-root-linux.log` retained.

Actual 503 PASS rows (38 top-level), 0 FAIL, 0 SKIP. Owned project `sandbox-target-journal-test-68272-1791322082`; exact project-label container/volume/network inventories all returned exit0 and zero resources. Script trap ran its exact-owned cleanup; cleanup removal commands suppress their own errors, so inventory independently confirms no resources remain (individual rm exits were not logged). Host race is separate; native CGO0 is not Linux race/power-loss/production isolation proof.

{
  "source": "4dd09c8f0c539a5729972ad498a6445a1b4161fa",
  "project": "sandbox-target-journal-test-68272-1791322082",
  "counts": {
    "PASS": 503,
    "FAIL": 0,
    "SKIP": 0
  },
  "top_PASS": 38,
  "cleanup_inventories": {
    "containers": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    },
    "volumes": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    },
    "networks": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    }
  },
  "actual_exit": 0,
  "attach_exit": 0,
  "script_exit": 0
}

## Complete saved root-final-gates-report.md

# Root frozen journal final verification gates

Source4dd09c8f0c539a5729972ad498a6445a1b4161fa.

- Host full-package race `go test -race ./internal/runtime/controltarget -count=1 -v`: actualexit0,38.468s,495PASS38top/0FAIL/2expectednonrootownershipSKIP. Full raw root-final-host-race.log.
- Actual canonical LinuxUID0 fullpackage at same source:503PASS38top/0FAIL/0SKIP,actualtest/attach/scriptexit0; task-3-root-linux-report/log/json retain exact project andzero cleanup inventories. No repeated native run on unchanged source.
- Host normal fulljournal package exercised in Root fullrepositoryrun below,30.141s PASS, nonverbose does not enumerate individualskipcounts. Worker focused andfullnormal source-specific logs separately retained.
- Fullrepository `go test -count=1 ./...` withfreshnativeetcd fixture usingown explicit root-full-repo-native.sh adapted from existing canonicalscript: actualexit0. Nonverbose output cannot establish per-testskip/warningcounts; nativeetcd endpoints/containers/foreign fixtureenv exported, prior alreadycertified nativefault/racebaseline unchanged. Exactowned project inventories below allzero; no fixture remains. Full rawroot-full-repo-native.log/json preserved.
- `go vet ./...`, `go build ./...`: actual0/no diagnosticoutput; ownroot-final-vet.log/root-final-build.log.

{
  "project": "sandbox-etcd-state-test-69171-1791322195",
  "cleanup_inventories": {
    "containers": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    },
    "volumes": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    },
    "networks": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    }
  },
  "fullrepo_exit": 0,
  "passed_packages": 29,
  "no_test_packages": 8,
  "etcd_package": "ok  \tgithub.com/goairix/sandbox/internal/storage/state/etcd\t117.057s"
}

## Complete saved final-fix-report.md

# Final fix implementation report — T3-M1

Source BASE: `7642fc17bb3e9df23dad6453272859b8ff5e02b3` on `codex/etcd-state-management`.
Frozen script commit: `34b9a03beed289fc97678a284fa6be7702d13082` (`test(runtime): require terminal journal fixture state`).

## Implementation and scope

Changed only `scripts/test-runtime-target-journal.sh` in the tracked source. A single actual Docker inspection now returns Status, Running, and ExitCode together. The script accepts only the exact `exited false <exit>` tuple with a canonical decimal process exit code in 0..255. Failed inspection keeps its nonzero inspection exit; incomplete, nonterminal or malformed inspection exits 2. The success/completion evidence line appears only after this validation and explicitly reports `state=exited running=false`. Valid nonzero actual test exits remain authoritative; actual exit zero with nonzero attach still returns the attach error.

The exact pinned local image, actual architecture inspection, compilation command, readonly root/binary, network none, UID 0, drop ALL/add CHOWN, no-new-privileges, owned labels/resources, cleanup/traps, and final actual exit propagation are unchanged. No Go, product config, image, dependencies, or tests changed. This own report is the only other file written.

## Actual commands and results

Working directory for all commands: `/Users/dysodeng/project/go/cloud/sandbox`.

- Read own `final-fix-brief.md` first, then the supplied exact final review finding, current full script, task-3 context Global Constraints, implementer prompt and verification-before-completion skill. Read commands exited 0. Initial combined context/review display was truncated; the exact T3-M1 finding and full script were visible, and a separate bounded context read plus separate complete prompt/skill read recovered the material used for this fix. No fresh whole-review EOF claim is made.
- `git status --short`, `git branch --show-current`, `git rev-parse HEAD`: exit 0; confirmed requested branch/BASE. Status named an existing unrelated Sentinel modification; its contents were not opened, changed or staged.
- `bash -n scripts/test-runtime-target-journal.sh`: exit 0, no output.
- `git diff --check -- scripts/test-runtime-target-journal.sh`: exit 0, no output.
- `git diff -- scripts/test-runtime-target-journal.sh`: exit 0; read the complete 13-insertion/2-deletion diff for self-review before commit.
- `git add -- scripts/test-runtime-target-journal.sh`: exit 0.
- `git diff --cached --check -- scripts/test-runtime-target-journal.sh`: exit 0, no output.
- `git diff --cached --name-only`: exit 0; only `scripts/test-runtime-target-journal.sh` staged.
- `git commit -m 'test(runtime): require terminal journal fixture state' -- scripts/test-runtime-target-journal.sh`: exit 0; created `34b9a03`, one file changed, 13 insertions and 2 deletions.
- `git rev-parse HEAD`: exit 0; full frozen SHA above.

No helper/build/verification error occurred. Syntax checking does not establish actual Docker behavior. No worker Docker call, fake Docker/mirror test, new test harness, repeated Go suite, subagent, configuration/secret access or sibling workspace access occurred. TDD was not required for this low-impact script evidence refinement; no RED/GREEN regression claim is made.

## Self-review

The exact tuple assertion rejects default-zero created/running states before test completion is labeled. Failed and missing inspection cannot produce a default successful result. Status and Running come from the same successful actual inspection; no race from separate field reads is introduced. Numeric validation prevents malformed or out-of-range exit data from reaching `exit`, with canonical decimal spelling preventing octal interpretation. Existing actual nonzero test results and attach failure handling are preserved. Scope stayed at the requested completion-evidence branch; no other fixture behavior was refactored.

## Root proof received and independently inspected

Root actually executed `bash -n scripts/test-runtime-target-journal.sh && bash scripts/test-runtime-target-journal.sh` at frozen HEAD `34b9a03beed289fc97678a284fa6be7702d13082`. The implementation worker did not execute Docker. Root supplied these exact own evidence files:

- `.superpowers/sdd/2026-10-07-runtime-target-journal/final-fix-root-linux.log`
- `.superpowers/sdd/2026-10-07-runtime-target-journal/final-fix-root-linux.json`
- `.superpowers/sdd/2026-10-07-runtime-target-journal/final-fix-root-linux-report.md`

The worker read the complete supplied JSON/report and final 12 raw log lines (read commands exit 0), then independently parsed all raw result markers using:

`awk '/^[[:space:]]*--- PASS:/ {passes++} /^--- PASS:/ {top++} /^[[:space:]]*--- FAIL:/ {failures++} /^[[:space:]]*--- SKIP:/ {skips++} END {printf "PASS=%d top_PASS=%d FAIL=%d SKIP=%d\n", passes, top, failures, skips}' .superpowers/sdd/2026-10-07-runtime-target-journal/final-fix-root-linux.log`

Actual command exit 0 and complete output: `PASS=503 top_PASS=38 FAIL=0 SKIP=0`. No whole-verbose-log manual EOF claim is made. The raw final evidence includes final test `PASS`, `Linux native test state=exited running=false actual_exit=0 attach_exit=0`, `Linux native top-level failures=0 skips=0 (host race is separate)`, and `ROOT_SCRIPT_EXIT=0`. Root reports direct script and wrapper exit 0; this supplies the required real successful terminal-state path at this exact script source.

`rg -ni 'warning|data race|panic' .superpowers/sdd/2026-10-07-runtime-target-journal/final-fix-root-linux.log` exited 0 with exactly one match, raw line 65: `journal_capacity_test.go:50: 64KiB authentic record fill: 50 records 55111B, warning=true; next 1095B would reach85% and was rejected, original retry/query/close available`. This is the expected measured capacity-state diagnostic, transparently reported as one raw warning marker and zero actual emitted test warnings. No DATA RACE or panic match occurred.

Exact owned project: `sandbox-target-journal-test-71964-1791323093`. Root's post-trap container, volume and network inventories each have command exit 0, empty remaining lists and empty stderr. These prove the independently observed empty inventories for this project. Individual `docker rm` exits are suppressed by the unchanged cleanup function and are not claimed individually successful.

After reading the actual evidence, `git rev-parse HEAD` exited 0 and still returned `34b9a03beed289fc97678a284fa6be7702d13082`; no source mutation followed freezing. Only this report was appended. The canonical native run exercised actual UID0 ownership tests and had zero skips. CGO0 native Linux execution is not Linux race, power-loss or production-isolation proof. Prior unchanged Go host-race/full-repository/vet/build gates remain Root-owned stored evidence; they were not repeated or relabeled worker execution. The actual real run establishes the accepted path; no fabricated failure-path runtime coverage is claimed.

## Final status

DONE for this single T3-M1 script completion-evidence fix. One script-only commit, syntax/diff/self-review verified, and actual canonical Linux terminal/exit/cleanup proof above. No outstanding concern or additional fix wave is proposed.

## Complete saved final-fix-root-linux-report.md

# Root final fix actual Linux execution

Actual canonical `bash -n scripts/test-runtime-target-journal.sh && bash scripts/test-runtime-target-journal.sh` at exact34b9a03beed289fc97678a284fa6be7702d13082 script-only fix, Go sourceunchanged4dd09c8. Wrapper/directscript actualexit0; completion explicitly requires successful sameinspection exited/Runningfalse/canonicalprocessExit0. 503PASS38top/0FAIL/0SKIP,0actualtestwarnings; one warning=true capacity-state diagnostic; actualownership casesrun. Rawfinal-fix-root-linux.log fully retained. Exactprojectposttrap inventories allactual0/empty; individualrmcodes suppressed/noindividualclaim. NativeCGO0 isnotLinuxrace/power-loss/productionisolationproof. UnchangedGo fullhostrace/repo/vet/build earlierRootsourcegatesremainapplicable; no unnecessaryrepeat.

{
  "source": "34b9a03beed289fc97678a284fa6be7702d13082",
  "project": "sandbox-target-journal-test-71964-1791323093",
  "counts": {
    "PASS": 503,
    "FAIL": 0,
    "SKIP": 0
  },
  "top_PASS": 38,
  "terminal_line": "Linux native test state=exited running=false actual_exit=0 attach_exit=0",
  "script_exit": 0,
  "cleanup_inventories": {
    "containers": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    },
    "volumes": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    },
    "networks": {
      "exit": 0,
      "remaining": [],
      "stderr": ""
    }
  },
  "warning_markers": 1,
  "expected_capacity_warning_diagnostics": 1,
  "actual_test_warnings": 0,
  "warning_classification": "Only match is journal_capacity_test.go:50 warning=true measured70pct state diagnostic; no emitted warning, panic or data race."
}

## Complete saved final-fix-review.md

**T3-M1 — Require actual terminal completion before accepting/labelling the native journal test result** — ADDRESSED. `scripts/test-runtime-target-journal.sh:80` now obtains Status, Running and ExitCode in one inspection. Lines 83–86 preserve failed-inspection rejection; lines 89–97 require exactly `exited false` and a canonical decimal exit in 0..255 before the completion label at line 98. A created/running state's default zero cannot be accepted. Lines 100–103 retain nonzero attach handling when actual exit is zero and retain actual test exit propagation.

### New Breakage in the Fix Diff

None. Critical: 0; Important: 0; Minor: 0. The anchored tuple check rejects incomplete, malformed and nonterminal observations. Canonical decimal validation avoids shell octal interpretation, and the range check rejects values above 255. The fix changes only the post-attach observation/validation/completion evidence branch. The diff leaves fixture creation, lifecycle/traps, actual test invocation and existing exit precedence intact; it contains no Go, image or product-configuration change.

### Out-of-Scope Observations

None. No previously accepted unit or old branch code was re-reviewed.

### Checks and Evidence

- Read the own final-fix brief first, the complete scoped re-review prompt, the complete implementer `final-fix-report.md`, and the complete supplied fix package `review-7642fc1..34b9a03.diff` once. Package names source BASE `7642fc17bb3e9df23dad6453272859b8ff5e02b3`, HEAD `34b9a03beed289fc97678a284fa6be7702d13082`, one script-only commit, 13 insertions and 2 deletions. No changed-file reread or Git command was needed.
- The implementer report names `bash -n`, working/staged `git diff --check`, complete diff self-review and script-only staging/commit, all with reported exit 0. These are reported implementer checks, not reviewer-executed tests.
- Read the complete root `final-fix-root-linux-report.md` and `final-fix-root-linux.json`, and the final 12 lines of the exact root raw log. They identify canonical execution at the same frozen HEAD. The raw ending confirms `PASS`, `Linux native test state=exited running=false actual_exit=0 attach_exit=0`, zero top-level failures/skips, and `ROOT_SCRIPT_EXIT=0`.
- Independently parsed the exact root raw log with `awk` for result markers: command exit 0; `PASS=503 top_PASS=38 FAIL=0 SKIP=0`. This agrees with root's JSON/report.
- Independently searched that exact log with `rg -ni 'warning|data race|panic'`: command exit 0; exactly one match at log line 65, the measured capacity-state diagnostic `warning=true` from `journal_capacity_test.go:50`. No DATA RACE or panic match. Root classifies zero actual emitted test warnings, consistent with the sole raw match being a capacity-state diagnostic.
- Root's JSON records the exact owned project `sandbox-target-journal-test-71964-1791323093`: post-trap container, volume and network inventory commands each exit 0 with empty remaining lists and empty stderr. This supports empty observed inventories; suppressed individual cleanup command exits are not separately proved.
- Two initial combined reads returned exit 1 because the dispatch's concatenated filename hints were interpreted as `diffreview-7642fc1..34b9a03.diff` and `final-fix-root-linux-report.json`, which do not exist. A narrowly filtered `rg --files` located the actual diff and JSON names. Both were then read successfully and completely. No review content was omitted due to those failed filename reads.
- No suite, Docker command, Git command, subagent, unrelated Sentinel/configuration/secret read, sibling-workspace access, or source mutation was performed. Only this explicitly requested own report was written.
- The actual native run proves the accepted successful terminal path. Rejection/attach-error/nonzero actual exit branches were assessed from the fix diff; no runtime failure-path coverage is claimed. Native CGO0 execution does not prove Linux race, power-loss or production isolation. Prior unchanged Go gates remain root-owned earlier evidence and were not rerun or recast as reviewer execution.

### Verdict

**Fix round:** All findings addressed, no new Critical/Important breakage. Findings addressed: 1/1; open: 0; new breakage: 0; out-of-scope observations: 0.
