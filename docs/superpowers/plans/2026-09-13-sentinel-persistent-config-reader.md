# Sentinel persistent configuration reader

## Scope and contract

Implement only `internal/redisbootstrap/persistent_*.go`, with pure
`ParsePersistentConfigs(c ClusterState, local Member, masterName string, redisConfig, sentinelConfig []byte) (PersistentConfigSnapshot, error)`.
The snapshot contains `State PersistedState`, `SentinelID string`, and `CurrentEpoch uint64`.
`State.SentinelEpoch` comes only from the monitored master's config-epoch; global
current-epoch and leader/vote epoch are not primary mapping epochs.

Caller must first validate private retained PVC files and their Configured identity,
and obtain a coherent read. Parsing bytes does not prove ownership, atomic reading,
remote authenticity, Redis live role, or permission to promote. No filesystem,
Redis, subprocess, transport, Chart, decision, state or attestation changes.

## Redis 7.4 grammar and conservative acceptance

Sources: [config.c](https://github.com/redis/redis/blob/7.4/src/config.c)
`loadServerConfigFromString`; [sds.c](https://github.com/redis/redis/blob/7.4/src/sds.c)
`sdssplitargs`; [sentinel.c](https://github.com/redis/redis/blob/7.4/src/sentinel.c)
`sentinelHandleConfiguration` and `rewriteConfigSentinelOption`.
Only blank/trimmed whole comment lines are skipped; # inside arguments is data.
Double quotes decode Redis escapes/hex; single quotes only escape apostrophe;
unquoted backslash is literal; a closed quote must end the token. Quote prefixes
inside otherwise unquoted tokens are permitted as in SDS. Decode then reject
controls and invalid UTF-8, even in noncritical/auth arguments. Errors disclose
only constant categories/file and line numbers, never directive names or tokens.
Directive and boolean case handling is byte-ASCII only, matching Redis rather
than Go Unicode folding; non-ASCII directive names are rejected. Legal Unicode
authentication values remain accepted and are never subjected to keyword folding.

Require one monitor of exact masterName, fixed DNS, port 6379 and quorum 2;
one config-epoch; one current-epoch >= config-epoch; one lowercase 40-hex myid.
Epochs are canonical decimal uint64. Require an explicit Redis port 6379 as a
minimum complete local configuration anchor, but do not claim to validate all
durability/auth settings. At most one replicaof/slaveof across both aliases;
fixed DNS, nonself and port 6379; it must equal monitor DNS. Its absence records
Primary only if monitor DNS equals local, never authorizing a role transition.
Reject include and executable config extensions. Validate optional announce DNS,
ports and hostname toggles if present. Allow normal rewrite Redis extra directives
after lexical validation. Sentinel extras use a known directive/arity allowlist:
master-scoped values must reference exact masterName, topology entries must use
fixed DNS and fixed ports, and IDs/epochs are strictly validated. Unknown Sentinel
subcommands are rejected even if their next argument happens to be masterName.
No missing known-replica/known-sentinel cardinality requirement: persisted views
can be partial. Duplicate topology endpoints and critical settings are rejected.
Bound each file to 1 MiB, each line to 64 KiB and 16384 lines.

Real rewrite compatibility refinement: CONFIG REWRITE/FLUSHCONFIG add the default
ACL user even when initial configuration used only requirepass. Accept at most one
exact `user default on sanitize-payload #<64lowerhex> ~* &* +@all` line, only when its
hash equals SHA256 of the same file's unique nonempty requirepass. Reject named
users, nopass, selectors, alternative privilege shapes, duplicate user/password
lines and external aclfile. This is a conservative built-in default-user subset,
not general ACL support or current server authentication proof.

## TDD implementation steps

1. Add table-driven parser contract tests and a temporary zero-result stub. Run
   focused tests and confirm behavioral RED, not a compilation accident.
2. Implement bounded lexical parsing and role/mapping evidence extraction to GREEN.
3. Add grammar differential examples from SDS and fail-closed configuration cases
   before tightening implementation; verify each new behavior RED then GREEN.
4. Add malformed bytes/decoded controls, duplicate aliases, foreign topology,
   unknown same-master subcommands, redacted errors and resource limit tests.
5. Run gofmt, focused/full package tests, package race, vet and available project
   golangci-lint. Report package and parser coverage plus limitations. No Docker,
   cluster operation or commit.

## Acceptance and limitations

Cover initial configuration, a nonzero promoted master, legal manually quoted
values with special passwords, separate epochs, accepted zero/max uint64 epochs,
missing myid/epochs/port, role/monitor conflict, stale IP/external/self addresses,
duplicate critical directives and aliases, foreign/noncritical Sentinel lines,
invalid quoting, raw NUL/Unicode controls, decoded controls and bounded input.
Parsing accepted Redis extra directives is not a full Redis config validator;
future coordinator must combine this snapshot with validated identity and live
authenticated evidence, never turn the snapshot into promotion authority.

`TestPersistentConfigsManuallyQuotedValuesAndExtraSettings` covers accepted input
grammar, not real Sentinel rewrite escaping. Redis 7.4 `sentinel.c` lines 1946–1958
emit auth-pass/auth-user with raw `%s`, and lines 2060–2063 emit sentinel-pass with
raw `%s`; unlike announce-ip they do not use `sdscatrepr`. Source-derived format
fixtures accept safe single-token credentials (including literal # and backslash)
and fail closed on raw credentials whose spaces or unbalanced quotes make the
result invalid. Their errors never disclose the broken raw credential. These are
source-derived fixtures, not evidence of a live CONFIG REWRITE run, and the parser
does not repair broken output or prove rewritten credential equality. Some raw
complex values could instead become syntactically valid but different tokens;
that risk remains outside this mapping reader and requires separate integration
work and an explicit generation/rewrite credential policy.

## Execution evidence

- First RED: focused tests failed on the intentionally zero-result stub for both
  valid snapshots and rejected invalid evidence; first GREEN passed.
- Second RED: additional boundary tests caught nonnumeric Sentinel settings and
  duplicate legacy/current announce aliases; canonical bounded numeric checks and
  alias normalization made the focused suite GREEN.
- SDS examples cover double/single quotes, literal unquoted backslash, prefix
  quote, empty value, invalid hex fallback, closing suffix and decoded controls.
- Review follow-up added accepted (config,current) epochs (0,0), (max,max),
  (0,max), and (max-1,max), asserting exact full snapshots. Added safe source-derived
  Sentinel 7.4 raw auth formats and six malformed raw format cases with redaction.
  All follow-up tests passed before production edits: this is coverage of existing
  functionality, not a newly observed RED or a new parser behavior. Renamed the
  manually quoted extras test and comments to avoid false real-rewrite evidence.
- Follow-up verification: package tests 90.7%, parser file 193/204 statements
  (94.6%), exported parser 93.8%; race and vet passed; lint 0 issues; diff-check
  clean. Production implementation did not change during this review follow-up.
- Quality follow-up RED: exported parser regressions accepted `Known-replica`
  and `resolve-hostnames yeſ` under Unicode folding. Replaced keyword/boolean
  folding with ASCII-only matching; both regressions became GREEN. Mixed ASCII
  case plus quoted Unicode authentication remains accepted. Empty/non-ASCII
  top-level keywords are also rejected. Initial fresh package tests/race passed,
  but final package/race runs after another implementation's concurrent files
  arrived failed only at challenge_test.go TestIdentityProofRoundTrip inventory/live
  with `not implemented`; those files are outside ownership and unchanged here.
  Final focused Persistent tests and race passed; vet passed and lint 0 issues.
  Parser file 205/215 statements (95.3%) after top-level keyword coverage.
  No broader password restriction or external changes.
- `go test ./internal/redisbootstrap -count=1 -coverprofile=...`: passed;
  package 90.3%, new parser file 191/204 statements (93.6%), exported parser 92.0%.
- `go test -race ./internal/redisbootstrap -count=1`: passed.
- `go vet ./internal/redisbootstrap`: passed.
- `golangci-lint run ./internal/redisbootstrap/...`: 0 issues.
- gofmt applied; `git diff --check`: clean. No Docker, network mutation or commit.

The remaining uncovered paths are mostly constant rejection arities and invalid
input identity/name paths; this is focused unit verification, not a live Redis
CONFIG REWRITE or cold-start acceptance test. Optional topology is partial by
design; legacy known-sentinel without runid and executable Sentinel scripts are
conservatively rejected. The numeric extra settings use canonical nonnegative
int32 bounds (positive except master-reboot-down-after-period), while mapping and
global epochs accept canonical full uint64.

## Later real rewrite regression (parent)

Using the cache-only, network-none `scripts/test-redis-bootstrap-rewrite.sh`, real
Redis 7.4.11 CONFIG REWRITE and Sentinel FLUSHCONFIG produced the protected default
ACL shape above. Both the actual integration test and a focused default-ACL unit
regression were observed RED (`unsupported Redis configuration extension`). The
strict hash-bound ACL refinement made focused Persistent tests and the actual
rewrite/cold-authentication fixture GREEN (0.54s). Missing/cross passwords remained
denied, and myid/role/epochs were retained. The fixture creates no volumes/networks,
verifies its one owned ephemeral container is gone and removes its test binary.
It is one member's config compatibility test, not replication/HA, wrapper, Chart,
same-name replacement or full-group recovery acceptance. Independent re-review
and full-source verification remain required before parent commit.
