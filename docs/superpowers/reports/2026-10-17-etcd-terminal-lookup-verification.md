# Released sandbox lookup verification

## Result and scope

PlainColdService.Lookup now returns NotFound only after authenticating the old
sandbox's completed release. A real native HTTP scenario passed GET 200 →
DELETE 202 → physical cleanup and GET 404. After a real AcquireIntent reused
the workspace, the old ID still returned 404 and the successor quartet's bytes
and MVCC metadata remained unchanged. This closes the terminal GET gap for
the explicit cold service; it does not complete the production migration.

The original four-point reader stays strict. Repeated Destroy behavior is
unchanged. The new public result is an observation, with no worker claim,
physical execution capability, quota authorization or workspace-free claim.

## Protocol and code boundaries

- Eight fixed read-only RPCs join placement, current quartet, immutable cleanup
  birth cohort, signed release, termination/provider completion and nine signed
  history points before a final pinned observation. There are no scans, retries,
  leases or proof caches. The deadline is eight request timeouts plus one second.
- Final confirmation pins 17 original history points, or 18 with a discovered
  successor owner. A successor is followed once into a final seven-point read;
  its canonical quartet and generation must agree. A third owner, ABA, revived
  old index/control, incomplete graph or invalid signature cannot become 404.
- A higher fence with no owner proves only the old ID's release. It does not
  establish the safety of another generation or authorize resource reuse.
- Strict flat response validation precedes protobuf sizing. Each transaction
  stays within 64 nodes and 256 KiB. The conservative aggregate reply envelope
  is 330614 bytes, below 512 KiB. Shared admission remains bounded at 256.
- Pure canonical/cohort/history validators are shared inside the existing etcd
  package. Existing worker wrappers preserve transport, admission and early
  validation order. Only this terminal observer retains the history snapshot;
  existing physical/quota proofs retain their previous memory behavior.
- The fixed wire-budget calculation caches only a validation error using
  sync.OnceValue, avoiding repeated maximal-envelope allocations on live GET.

Manual review covered comments, logical spacing, package cohesion, retained
pins and lifecycle. Read-only gofmt -d and git diff --check were clean.

## Host, race and Linux verification

All epochs use private caches and GOMAXPROCS=2. Root independently checked all
17 Go input categories, source/tool archives, before/after/current hashes and
artifacts before subsequent Go runs or source changes. Input counts below are
build dependency manifests, not test or capacity counts.

| Epoch | Result | Scope / inputs |
| --- | --- | --- |
| terminal-read-red-01 | Expected compile RED | Missing approved API only; 9079 inputs |
| terminal-read-host-01 | PASS | 16 top-level groups / 171 runs; 9082 inputs |
| terminal-read-race-01 | PASS | New and affected old protocol: 76 groups / 379 runs; 9087 inputs |
| terminal-read-vet-01 | PASS | Linux etcd vet; 9176 inputs |
| terminal-business-race-01 | PASS | Lookup / business Lookup / Destroy, seven groups; 9070 inputs |
| terminal-native-build-01 | PASS | Linux controlrunner test ELF; 9367 inputs |
| terminal-native-cli-01 | PASS | Main sandbox application compile; 11345 inputs |
| terminal-native-worker-01 | PASS | Actual sandbox-cleanup-worker compile; 9154 inputs |
| terminal-native-vet-01 | PASS | Linux controlrunner vet; 9367 inputs |

The main application compile is separate from the dedicated cleanup-worker
binary used below. The final native-only log correction is included in the
native builds and vet; it does not change the previously tested Go behavior.
The host positive proof uses real ed25519 signatures and the production
verifier, with six historical values represented by signed-stamp model data.
It is metadata-model coverage, not physical cleanup evidence.

## Actual native HTTP and worker evidence

`terminal-native-local-01` passed in 33.00 seconds. Raw log: 9430 bytes,
SHA-256 `7974dc2413cc4270ef45a8b5a3751e44d8fe9f95d67b95f783d62efa1c01bbe3`.
Runner ELF SHA-256:
`0aa21099a878381084119625adcd3db64239a05982a486e1b9937fd6fb352720`.
Dedicated worker ELF SHA-256:
`d78bd8a31a4bb61c377322a66cb7f50c985cb131b47231284ba3abafd570ebc4`.

The actual worker ran as UID 1000 with all five capability sets empty. Original
RunJoined was true, signed release revision was 143, P was gone, archive/modes
were verified, and the worker's original Wait returned exit 0. All 19 original
service bridges joined IO, Cmd.Wait and Success. One final WebSocket Close frame
reported EPIPE only after those successful joins; the strict existing tail
classification was unchanged. The original launcher was SIGKILLed and physically
joined. The private TLS etcd process was terminated and joined.

Root independently verified command/binary hashes, full raw log, before/after
container state, resource/security configuration and exact-name cleanup. Exit
was 0, without timeout or OOM. Container isolation used network none, private
cgroup, read-only root, 1 GiB memory, two CPUs and 256 PIDs; all capabilities
were dropped with seven setup capabilities explicitly restored. The worker
dropped them before execution. Four ELF bind mounts were read-only. The runner
container was removed and its exact-name inventory was empty.

## Remaining boundaries

This is one actual local-terminal HTTP scenario using a provider HTTP fixture,
not a Kubernetes cluster, full FUSE mount, automatic quota return or detached
cleanup. Cold quota remains occupied until the separate automatic quota wiring
is implemented. No old accepted native matrices were replayed. There is no
10k/100k/1M population, long-soak or full performance/capacity acceptance here.
Redis remains the default state path; production migration and switch remain
open. The implementation and shared extraction are submitted separately.
