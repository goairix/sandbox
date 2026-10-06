# Native issuer registry 验证证据

最终源 `67129ab46f34a8a086c6fd2218a64d6221f51b2c`；scope 只包括当前 issuer prerequisite，生产 Manager 仍 Redis。独立任务 gate 与 whole/scoped gate 均通过。真实 source reading 和全部1–42边界/裁决见同目录 final-review 文档。两次产品小提交已完成。以下记录 distinguish 实际 RED/GREEN、最初测试选择修正、完整 native/fullgates、historical failures、cleanup 和有限证明范围。

## 修复实施实际报告（原文）

Status: DONE. Both authorized slices are implemented, verified and separately committed. No product concerns found in scoped self-review. Root owns the independent scoped review and fresh final native/all-repository gates; this report does not claim those gates passed.

Workspace: `/Users/dysodeng/project/go/cloud/sandbox`; branch: `codex/etcd-state-management`.

Original review BASE: `0e2a67edacfc5c1edf8cee554b053695726ea5ef`.

Final source freeze: `67129ab46f34a8a086c6fd2218a64d6221f51b2c`.

## Scope and implementation

1. `publication_authority.go`: add `reflect.Chan` to the existing nil-capable reflection-kind case. Defined nil channel implementations of AuthorityClock now return ErrInvalidConfiguration without Observe. No clock calls, provider calls, new configuration or hostile-clock machinery were added. `publication_authority_test.go` adds a nil-channel clock whose Observe panics and checks publicationVerifier, validateOptions and New rejection; `command_authority_test.go` extends the existing configuration table with the same clock and unused issuer whose Certificate/SignStart panic. All-nil metadata-only and copied-root/exact-binding regressions remain green.
2. `mutation.go`: reserve first namespace-relative segment `command-issuers` in reservedStageKey, applying to all generic Stage Put/Delete paths through existing prepareMutation. Existing meta/attempt/stage protections remain identical, and comparison validation is untouched. `stage_test.go` checks the bare family, canonical UUID child and deeper descendant for Put and Delete. prepareMutation returns ErrInvalidMutation, zero Mutation and empty digest. BeginStage uses a counted rejecting Grant fake and an unconfigured KV client: inputs reject with zero grants and no key RPC. Allowed family/point/deep comparisons plus an unrelated business write remain accepted and copied; prefix comparison key/range, value comparison bytes and business write bytes survive caller mutation. Near-prefix `command-issuers-other` and its child remain writable/deletable.

Only the five permitted existing product files changed. No registry RPC, codec, SignStart, dependencies, Manager, image, M2 response-copy regression or legacy decoder/birth protocol changes. The copied-root publication integration test was untouched. Typed Register CAS and permanent-record behavior were untouched. No raw-etcd-writer or safe-GC claim is made; future GC requires its dedicated typed protocol.

## Actual process and setup

- First read `.superpowers/sdd/2026-10-07-etcd-command-authority/final-fix-brief.md` (complete requirements); no earlier plan or sibling workspace was read. The brief already contained all required evidence, so final-review.md was not needed.
- Initial `git status --short`, `git branch --show-current`, `git rev-parse HEAD` confirmed the branch/BASE and one unrelated tracked edit: `docs/superpowers/plans/2026-09-17-sentinel-kubernetes-129-compatibility.md`. That file was neither edited nor staged.
- Read `/Users/dysodeng/.codex/skills/using-superpowers/SKILL.md`; its dispatched-subagent stop applies. Read test-driven-development and verification-before-completion skills and the writing-good-tests reference. Explicit dispatch limits override the skill's broad-suite suggestion; Root owns full gates.
- Checked ancestor AGENTS.md paths from filesystem root through cwd; none existed. `rg --files internal -g AGENTS.md` returned exit 1/no matches. Used rg to locate the local authority/Stage/options entry points and tests, then read the five scoped files and the relevant local `client.go`, `client_test.go`, `command_authority.go`, `stage.go`, `stage_attempt_test.go` sections to understand preflight/dial/Grant ordering. No credentials or historical fixture ports were accessed.
- Two harmless read-path mistakes were observed and corrected: `cat /Users/dysodeng/.codex/skills/writing-good-tests.md` returned file-not-found; the correct reference `/Users/dysodeng/.codex/skills/test-driven-development/writing-good-tests.md` was then read. `sed -n '1,180p' internal/storage/state/etcd/backend.go` returned file-not-found; Backend is in the already-read `client.go`. These were setup reads, not test failures, and caused no mutations.
- No fixture started, no native/full-fault/full-repository worker suite run, and no subagents spawned. No approval or user question was requested.

## Slice 1 — clock RED → GREEN → small commit

Tests were written before production change and formatted with:

```sh
gofmt -w internal/storage/state/etcd/publication_authority_test.go internal/storage/state/etcd/command_authority_test.go
```

Actual RED commands:

```sh
go test ./internal/storage/state/etcd -run '^(TestPublicationRejectsNilChannelAuthorityClock|TestExecCommandAuthorityConfiguration/nil_channel_clock)$' -count=1
go test ./internal/storage/state/etcd -run '^TestExecCommandAuthorityConfiguration$/^nil_channel_clock$' -count=1
```

First RED exited 1 (`FAIL ... 2.208s`), publication test expected ErrInvalidConfiguration but got nil. The first combined pattern selected the publication test only; the separate second command explicitly selected the exec subtest. Second RED exited 1 (`FAIL ... 0.402s`), exec factory also expected ErrInvalidConfiguration but got nil. Both failures were actual behavior assertions, not compilation errors or panics. Fatal assertions stop before New on RED, avoiding dial/provider invocation. On GREEN, New follows successful rejection assertions and returns its validateOptions error before reaching client construction.

Applied only the reflect.Chan production addition; formatted the three slice files:

```sh
gofmt -w internal/storage/state/etcd/publication_authority.go internal/storage/state/etcd/publication_authority_test.go internal/storage/state/etcd/command_authority_test.go
go test ./internal/storage/state/etcd -run '^(TestPublicationRejectsNilChannelAuthorityClock|TestPublicationTrustConfiguration|TestExecCommandAuthorityConfiguration|TestOptionsValidation|TestOptionsTLSCertificateValidation|TestOptionsRejectsInvalidRestoreEpochIdentifiers)$' -count=1
go test -race ./internal/storage/state/etcd -run '^(TestPublicationRejectsNilChannelAuthorityClock|TestPublicationTrustConfiguration|TestExecCommandAuthorityConfiguration|TestOptionsValidation|TestOptionsTLSCertificateValidation|TestOptionsRejectsInvalidRestoreEpochIdentifiers)$' -count=1
go vet ./internal/storage/state/etcd
git diff --check
git diff -- internal/storage/state/etcd/publication_authority.go internal/storage/state/etcd/publication_authority_test.go internal/storage/state/etcd/command_authority_test.go
gofmt -l internal/storage/state/etcd/publication_authority.go internal/storage/state/etcd/publication_authority_test.go internal/storage/state/etcd/command_authority_test.go
```

GREEN regular exited 0 (`ok ... 0.593s`); race exited 0 (`ok ... 1.703s`); vet exited 0, no output; diff check exited 0, no output; gofmt -l exited 0, no paths. Read the complete actual diff. Self-review confirmed the one-kind fix reaches exec via existing paired validation, nil optional dependencies are unchanged, no provider method is invoked and the constructor validates before clientv3.New. Existing exact binding/copied roots and all-nil metadata-only cases ran in the exec configuration test. No native integration regression was implied by these results.

Immediately staged only this coherent slice, checked its index and committed before beginning slice 2:

```sh
git add -- internal/storage/state/etcd/publication_authority.go internal/storage/state/etcd/publication_authority_test.go internal/storage/state/etcd/command_authority_test.go
git diff --cached --check
git diff --cached --stat
git commit -m 'fix(etcd): reject nil channel authority clocks'
```

All exited 0. Commit `d82e276b982e8f7c887f7a422f32897d043f042e`: 3 files, 24 insertions, 2 deletions. No other staged files.

## Slice 2 — reservation RED → GREEN → small commit

Wrote the direct preflight, BeginStage pre-Grant and allow/copy/near-prefix tests before changing reservedStageKey:

```sh
gofmt -w internal/storage/state/etcd/stage_test.go
go test ./internal/storage/state/etcd -run '^TestMutationReservesCommandIssuerWrites$' -count=1
go test ./internal/storage/state/etcd -run '^(TestBeginStageRejectsCommandIssuerWritesBeforeGrant|TestMutationAllowsCommandIssuerComparisonsAndNearPrefixWrites)$' -count=1
```

Direct RED exited 1 (`FAIL ... 1.685s`): all six cases expected ErrInvalidMutation but got nil. The next command exited 1 (`FAIL ... 0.423s`): all six BeginStage cases returned `ErrOutcomeUnknown: grant stage lease: unexpected lease grant for reserved issuer key`, demonstrating actual late validation through the counted Grant fake. No panic/network/native fixture was needed for either RED. Allowed comparisons/copied payload/near-prefix cases produced no failures in that command.

Applied only the namespace-relative first-segment reservation and ran:

```sh
gofmt -w internal/storage/state/etcd/mutation.go internal/storage/state/etcd/stage_test.go
go test ./internal/storage/state/etcd -run '^(TestMutation.*|TestBeginStageRejectsCommandIssuerWritesBeforeGrant|TestReceiptEncoderRejectsOversizedReference|TestStageAttemptLocatorValidate)$' -count=1
go test -race ./internal/storage/state/etcd -run '^(TestMutation.*|TestBeginStageRejectsCommandIssuerWritesBeforeGrant|TestReceiptEncoderRejectsOversizedReference|TestStageAttemptLocatorValidate)$' -count=1
go vet ./internal/storage/state/etcd
git diff --check
git diff -- internal/storage/state/etcd/mutation.go internal/storage/state/etcd/stage_test.go
gofmt -l internal/storage/state/etcd/mutation.go internal/storage/state/etcd/stage_test.go
```

GREEN regular exited 0 (`ok ... 0.614s`); race exited 0 (`ok ... 1.720s`); vet, diff check and gofmt -l exited 0 with no output. Full selected existing mutation budget/unsafe-write comparisons, receipt encoder bound and pure Stage locator regression ran. Read the complete diff. Self-review confirmed exact segment equality, bare/descendant reservation, unchanged meta/attempt/stage branches, unmodified comparison path, zero capability/digest for invalid writes, zero Grant and absence of KV dependency use. The allowed comparison/copy test is specifically required for this reserved-family ruling, not the deferred Stage M2 regression.

Immediately staged only slice 2, checked its index and committed:

```sh
git add -- internal/storage/state/etcd/mutation.go internal/storage/state/etcd/stage_test.go
git diff --cached --check
git diff --cached --stat
git commit -m 'fix(etcd): reserve command issuer keys from generic stages'
```

All exited 0. Commit `67129ab46f34a8a086c6fd2218a64d6221f51b2c`: 2 files, 84 insertions, 1 deletion. No large final commit was created.

## Final scoped inspection / handoff

```sh
git log -2 --format='%H %s'
git status --short
git diff 0e2a67edacfc5c1edf8cee554b053695726ea5ef..HEAD --check
git diff 0e2a67edacfc5c1edf8cee554b053695726ea5ef..HEAD --stat
```

All exited 0. The combined BASE-to-freeze diff changes exactly the five authorized files: 108 insertions, 3 deletions. Combined diff check is clean. Working tree still shows only the preserved unrelated Sentinel tracked edit; the task-local report is allowed scratch documentation and is not part of either product commit. Source was frozen before report writing and Root was notified to begin its independent work against the original BASE.

Concerns: none in the scoped product fixes. Proof limits: no literal dial hook instrumentation; constructor rejection is exercised with otherwise-valid options and verified against actual validate-before-client source ordering. Panic clock/issuer methods detect any forbidden invocation in these tests. No native fixture or full repository gate was run by this worker, by explicit dispatch scope. Root must record the independent scoped review and fresh final gates after this source freeze. Prior M2 remains assigned to the later Stage/effect unit.

## Controller 修复后实际完整验证（原文）

Source frozen `67129ab46f34a8a086c6fd2218a64d6221f51b2c`.

Native42388 / repo86542 / vet66169 / build46949 all actual exit0. Native new owned script project, no historical ports; repo separate fresh properly named/inventoried manual cluster. Prior failed initial0e2 fixture-name guard run remains recorded in controller-final-gates.md; it is not erased by these post-fix successes.

{
  "native_top": {
    "PASS": 230
  },
  "native_completion": ["ok github.com/goairix/sandbox/internal/storage/state/etcd 98.792s"],
  "expected_warnings": {
    "rpc error: code = NotFound desc = etcdserver: requested lease not found": 38,
    "rpc error: code = Canceled desc = context canceled": 3,
    "rpc error: code = ResourceExhausted desc = etcdserver: mvcc: database space exceeded": 2
  },
  "script_cleanup": {
    "sandbox-etcd-state-test-37373-1791310785": {
      "containers": 0,
      "volumes": 0,
      "networks": 0
    }
  }
}

Full repository results:

```
ok  	github.com/goairix/sandbox/cmd/apparmor-loader	(cached)
ok  	github.com/goairix/sandbox/cmd/redis-bootstrap	(cached)
ok  	github.com/goairix/sandbox/cmd/sandbox	(cached)
ok  	github.com/goairix/sandbox/cmd/workspace-mounter	(cached)
?   	github.com/goairix/sandbox/cmd/workspace-probe	[no test files]
ok  	github.com/goairix/sandbox/internal/api	(cached)
ok  	github.com/goairix/sandbox/internal/api/handler	(cached)
?   	github.com/goairix/sandbox/internal/api/middleware	[no test files]
ok  	github.com/goairix/sandbox/internal/apparmorloader	(cached)
ok  	github.com/goairix/sandbox/internal/config	(cached)
ok  	github.com/goairix/sandbox/internal/fuseprotocol	(cached)
ok  	github.com/goairix/sandbox/internal/imageref	(cached)
ok  	github.com/goairix/sandbox/internal/kubecontract	(cached)
?   	github.com/goairix/sandbox/internal/logger	[no test files]
ok  	github.com/goairix/sandbox/internal/mounter	(cached)
ok  	github.com/goairix/sandbox/internal/redisbootstrap	(cached)
ok  	github.com/goairix/sandbox/internal/runtime	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/controlprotocol	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/docker	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/kubernetes	(cached)
ok  	github.com/goairix/sandbox/internal/sandbox	(cached)
ok  	github.com/goairix/sandbox/internal/storage	(cached)
ok  	github.com/goairix/sandbox/internal/storage/state	(cached)
ok  	github.com/goairix/sandbox/internal/storage/state/etcd	82.873s
ok  	github.com/goairix/sandbox/internal/storage/state/redis	(cached)
?   	github.com/goairix/sandbox/internal/telemetry	[no test files]
?   	github.com/goairix/sandbox/internal/telemetry/log	[no test files]
ok  	github.com/goairix/sandbox/internal/telemetry/metrics	(cached)
?   	github.com/goairix/sandbox/internal/telemetry/trace	[no test files]
ok  	github.com/goairix/sandbox/internal/workspaceprobe	(cached)
?   	github.com/goairix/sandbox/pkg/types	[no test files]
ok  	github.com/goairix/sandbox/test/integration/api	(cached)
ok  	github.com/goairix/sandbox/test/integration/fuserefillelease	(cached)
ok  	github.com/goairix/sandbox/test/integration/helm	(cached)
ok  	github.com/goairix/sandbox/test/integration/workspacefuse	(cached)
?   	github.com/goairix/sandbox/var/tmp/cce	[no test files]
```

Vet/build logs empty, actual exit0 observed via completed sessions. diffcheck PASS; Sentinel preserved. Scoped independent fix review now PASS, both addressed; manual cleanup completed below.

Manual fixture down actual exit0; exact project sandbox-etcd-state-test-36627-1791310578558630000 label counts {"containers": 0, "volumes": 0, "networks": 0}. All own native/manual fixtures gone; postfix-fixture.env ports INVALID historical evidence only. No running tests/worker before cleanup. Fullrepo also discovered an unrelated ignored local var/tmp/cce Go package with no test files; no source/credentials there read or changed.

## 修复前验证与失败事实

### 原冻结0e2a67e阶段的历史记录

Sourcefrozen0e2a67edacfc5c1edf8cee554b053695726ea5ef. Full58commits131files23929insertions12deletions25124lines1348151B package.

Freshownednativefault-race script session27913 exit0: {'PASS': 226, 'SKIP': 0, 'FAIL': 0}; completion ['ok  \tgithub.com/goairix/sandbox/internal/storage/state/etcd\t99.647s']; warnings Counter({'rpc error: code = NotFound desc = etcdserver: requested lease not found': 38, 'rpc error: code = Canceled desc = context canceled': 3, 'rpc error: code = ResourceExhausted desc = etcdserver: mvcc: database space exceeded': 2}) expected injectedlease/cancel/NOSPACE paths. Project sandbox-etcd-state-test-30403-1791307924 trapcleanup labels{'containers': 0, 'volumes': 0, 'networks': 0}. Log /tmp/etcd-command-authority-owned.log. No DATA RACE marker.

Vet69906/build15853 exit0emptylogs, diffcheckPASS, sourceunchanged/Sentineldirtypreserved.

INITIALfullallrepo65421 FAILEDexit1 etcd80.592s /tmp/etcd-command-authority-repo.log: TestStageNOSPACEResolverKeepsUnknown andTestStageSurvivesFixtureLeaderFailure guard invaliddisposablefixtureproject. CONTROLLER generatedmanualname sandbox-etcd-state-authority-ec2d797185d5 incompatible exact fixtureProjectPattern; bothfailedbeforefault. SystematicDebugging traced sourceenv/name->regex->Fatal, correctedENVIRONMENTONLY bynewownedproject sandbox-etcd-state-test-30904-1791308070559279000. Oldmanualremovedlabels0allresources, noownershiprules weakened/no productsource changes. Actualnativefullracealreadyusesproperfreshscriptproject andpassesbothcases.

Correctedfullallrepo source final-fixture.env (newproperownedproject/status/ports final-fixture.md) currentlyRUNNING session30976, log /tmp/etcd-command-authority-repo-corrected.log, exitPENDING. DoNOTclaim allrepo passing untilcontrollercompletion notification/factsupdate. This is fullrerun afterconcreteENVfix, initialfailnoterased/nottreatedasflakyisolatedrepeat.

CORRECTEDallrepo30976 exit0: /tmp/etcd-command-authority-repo-corrected.log, actualnativeetcdtestduration recorded below; allpackage results inspected0FAIL. Fullrerun afterownedfixtureENVsourcecorrection, notisolatedrepeat; productsourcefrozen0e2a67e unchanged. Originalfailedlog/2ownershipguardfailure retainedabove. Nativefault-race226PASS0SKIP0FAIL99.647s plusvet/build/diff allpass. Next manualfixturecleanup exactproject pendingrootaction; newportsinvalidwhenremoved.

ok  	github.com/goairix/sandbox/cmd/apparmor-loader	(cached)
ok  	github.com/goairix/sandbox/cmd/redis-bootstrap	(cached)
ok  	github.com/goairix/sandbox/cmd/sandbox	(cached)
ok  	github.com/goairix/sandbox/cmd/workspace-mounter	(cached)
ok  	github.com/goairix/sandbox/internal/api	(cached)
ok  	github.com/goairix/sandbox/internal/api/handler	(cached)
ok  	github.com/goairix/sandbox/internal/apparmorloader	(cached)
ok  	github.com/goairix/sandbox/internal/config	(cached)
ok  	github.com/goairix/sandbox/internal/fuseprotocol	(cached)
ok  	github.com/goairix/sandbox/internal/imageref	(cached)
ok  	github.com/goairix/sandbox/internal/kubecontract	(cached)
ok  	github.com/goairix/sandbox/internal/mounter	(cached)
ok  	github.com/goairix/sandbox/internal/redisbootstrap	(cached)
ok  	github.com/goairix/sandbox/internal/runtime	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/controlprotocol	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/docker	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/kubernetes	(cached)
ok  	github.com/goairix/sandbox/internal/sandbox	(cached)
ok  	github.com/goairix/sandbox/internal/storage	(cached)
ok  	github.com/goairix/sandbox/internal/storage/state	(cached)
ok  	github.com/goairix/sandbox/internal/storage/state/etcd	76.840s
ok  	github.com/goairix/sandbox/internal/storage/state/redis	(cached)
ok  	github.com/goairix/sandbox/internal/telemetry/metrics	(cached)
ok  	github.com/goairix/sandbox/internal/workspaceprobe	(cached)
ok  	github.com/goairix/sandbox/test/integration/api	(cached)
ok  	github.com/goairix/sandbox/test/integration/fuserefillelease	(cached)
ok  	github.com/goairix/sandbox/test/integration/helm	(cached)
ok  	github.com/goairix/sandbox/test/integration/workspacefuse	(cached)

Correctedmanual finalfixture down exit0; exactproject sandbox-etcd-state-test-30904-1791308070559279000 labels0containers0volumes0networks verified. All Task2/initialmanual/correctedmanual/script fixtureports nowINVALIDhistoricalonly; no controller-owned fixture resources remain. All worker/test sessions completed beforecleanup.

