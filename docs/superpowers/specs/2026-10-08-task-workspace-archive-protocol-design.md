# Task workspace archive protocol design

This implements the approved final-sync requirement's protocol boundary. The previous physical source reads actual user-private files; this unit defines independently authenticated task attribution and bounded archive framing. It does not wire Supervisor, target journal, etcd signer or streaming receiver, and grants no live metadata or provider authority.

## Decisions and alternatives

Keep the existing five-second handshake/request limit. A new signed ticket has a start window of at most30 seconds and a separate fixed `SessionNotAfter`, strictly later than the start window and at most1 hour after `NotBefore`. The whole session must fit the issuer certificate. Future target admission selects immutable birth/activation UID/GID; request headers have no filesystem path or UID/GID override.

Reusing exec/TAR from a user process would bypass permanently closed USERS. Extending an old quiescence ticket would revive expired authority. Use a distinct fixed `task_workspace_archive` purpose, original final-sync attempt identity and independently expected attribution instead. This is an elaboration of the approved parent plan, with autonomous small commits already authorized, not a new deployment or trust grant.

## Protocol context

`TaskWorkspaceArchiveContext` contains:

- `Current TaskCloseDataContext`: current original task/claim, exact runtime, control/data epoch, issuer; `CommandID` is the immutable final-sync attempt UUID.
- `QuiescenceContext TaskUserQuiescenceContext`: the historical CloseData/quiescence lineage.
- `QuiescenceTicketDigest`, `QuiescenceReceiptDigest`, `PlanDigest`: lowercase64-character hashes.
- `QuiescenceIntentRevision`, `FinalSyncRevision`: positive immutable metadata revisions.

Reuse existing pure context validation. Current and historical quiescence share trust binding, taskID/digest, sandbox/workspace/generation/runtime, control revision and data epoch. Current claim create revision cannot precede the historical one; equality requires identical claimID/worker/lease. Archive commandID differs from both historical close and quiescence commandIDs. Quiescence intent revision exceeds historical quiescence claim create revision and CloseData intent revision. Final-sync revision exceeds both current claim create revision and quiescence intent revision. These checks validate attribution only; they cannot establish actual immutable KV birth, live claim or drained operations.

Fixed purpose implies archive for `sync`; `fuse`/`none` have no archive ticket in this unit. Future backend must establish that reserved mode and the exact plan digest before signing; protocol does not infer mode from a hash.

## Signed ticket and terminal attribution

`TaskWorkspaceArchiveTicketClaims`: Version1, Purpose `task_workspace_archive`, Context, NotBefore/NotAfter, SessionNotAfter. Domain `sandbox-task-workspace-archive-ticket:v1\x00`, wire maximum8192 bytes. Signer uses the verified current issuer delegate and matching private key; exact binding/certificateID/digest and the entire session window must fit the issuer certificate. Verifier authenticates the issuer freshly, requires independently expected context and uses the existing one-second start-window uncertainty boundary. Evidence copies normalized wire and returns attribution/window/digest only.

`TaskWorkspaceArchiveReceiptClaims`: Version1, State `archive_complete`, Context, TicketDigest, the same three times, ArchiveBytes/EntryCount uint64 and SHA256. Domain `sandbox-task-workspace-archive-complete:v1\x00`, wire maximum16384 bytes. ArchiveBytes is a multiple of512, between1536 and1TiB inclusive; EntryCount1..1000000; hashes are lowercase64 hex. Pure signing asserts supplied attribution only; the future original live producer must actually stream and finish the source before signing.

`VerifyTaskWorkspaceArchiveReceipt` takes wire, runtime certificate, independently expected context/ticket digest and all three expected times, plus current trusted time. It freshly authenticates the exact runtime, requires its certificate to cover the full original session window, exact attribution/times, and correct runtime signature. Historical verification after ticket/session expiry is permitted while the exact runtime certificate is current. Returned summary is signed attribution, not verified received bytes, current execution authority, remote settlement or safe-release proof. Future receiver must compare actual byte count/digest/entries and consume actual response EOF.

Use existing strict recursive JSON schemas: reject duplicate/unknown/null/wrong-type/missing fields and oversized wire, normalize harmless whitespace for signature verification. Reuse existing issuer/private-key/schema helpers; avoid duplicated existing validation bodies. Evidence Wire getters clone.

## Standalone byte codec

`TaskWorkspaceArchiveEnvelope`: Version1, Purpose `task_workspace_archive`, Context, TicketDigest, Ticket, IssuerCertificate, OriginalQuiescenceReceipt. Structural frame validates fixed purpose/hash and nonempty bounded ticket<=8192/certificate<=4096/receipt<=12288. Context semantics/authentication remain in the protocol verifier. No query, arbitrary command, path, UID/GID or caller-chosen root.

`WriteTaskWorkspaceArchiveRequest` / `ReadTaskWorkspaceArchiveRequest` use uint32 big-endian bounded JSON length<=existing MaxHeader131072, canonical JSON, exactly one request and actual EOF. Reject trailing bytes, alternate purpose, noncanonical JSON, duplicate/unknown/null or missing required fields, nil/typed-nil IO and truncation. Do not modify existing RuntimeRequest router, Session, exec/close/quiescence codecs or Supervisor in this unit.

Separate archive response frames have one kind byte plus uint32 big-endian payload length. `ArchiveData=0xa1`:1..32768 raw bytes. `ArchiveComplete=0xa2`:1..16384 signed receipt bytes. `WriteTaskWorkspaceArchiveFrame` / `ReadTaskWorkspaceArchiveFrame` validate kind and length before IO/allocation, propagate truncation, short writes and IO errors. They operate on one frame only; they do not establish stream ordering, cumulative bounds, digest, TLS, actual EOF or authenticated completion. Existing event kinds and MaxEvent remain unchanged.

## Acceptance and remaining integration

Tests use independent standard-library signatures to bypass production validators. Prove valid ticket/receipt, immutable evidence copies, exact start-window boundaries, fixed session deadline and issuer/runtime certificate limits; mutate every context/time/summary binding, wrong purpose/domain/key/cert, malformed/oversized JSON and historical completion. Standalone frame tests cover actual binary payloads, size boundaries, request EOF/canonicality, truncation and writer errors. No source opens or archive bytes may be sent by this unit.

Run focused meaningful RED with compilable reject stubs, then affected protocol/transport race once and vet. Preserve raw command argv/times/exits/output hashes, source hashes, selected Go/platform env only; never whole env. No previously accepted native fixture replay. Independent task review before acceptance; exact eight source paths and separate tracked acceptance commits.

Remaining: target durable original archive admission/completion and cold refusal; Supervisor private original producer, permanent gates and physical joins; fixed birth user source; short authenticated header plus bounded stream session; current etcd claim/reservation fences at each send; actual receiver count/digest/EOF; per-provider attempt settlement, exact termination and safe release. Production wiring/capacity/deployment/Redis removal remain incomplete.
