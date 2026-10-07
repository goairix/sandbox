# Task CloseData etcd review acceptance

Task original BASE `cb5b9002d07759627fbfd96a586fcdea99124fb6` → independent review HEAD `f6ed42aa4fcd007e6d292429c5c6fab1a00ff652`; candidate `0627000742785536f0d28835c596a308d42a61c2`. Root later verification-only commit `47483ba` changes no product. Complete17-owned-file original-range diff2092lines/81596bytes, SHA256 `f05960e84f888fc7b2f204284f5b645ab032e58a3e613f011b4f6750342f8f8f`;12 neighboring exclusions explicit. Final new-unit review must use unfiltered whole range.

Independent reviewer launcher authored no current-unit product; spec compliant, quality Approved, C0/I0/M4,23CV. Root read the entire final report to EOF and accepts Task3 with each disposition below; no product fix or test replay required. Full report retained at OWN/task-3-review.md, SHA256 `d7016a66928951af4dfe6f4c0fa430accc2592e2d71bc57dc2fd32d1d9da726a`. Verification is in `2026-10-07-task-close-etcd-verification.md`; all raw evidence/failures remain in OWN. Source commits remain separately reviewable, no push/merge/deploy.

## Root Minor dispositions

| Item | Disposition and consequence |
|---|---|
| M1 repeated lease cleanup | Accepted finite evidence limitation. Root already classifies exact7 LeaseRevoke NotFound client WARN and21 replicated lease_revoke apply WARN, preserves raw and primary/cleanup separation. Do not suppress or weaken lost-reply/idempotent cleanup coverage; strict expected-log checks belong to future fixture maintenance. |
| M2 expiry DeadlineExceeded | Accepted classified fault. This WARN is not CAS proof; separate fresh-context exact old native transaction with still-live Stage guard proves missing original claim refusal. No replay or stronger inference. |
| M3 baseline fixture warnings | Accepted isolated fixture scope only. Exact112 WARN classified as56 same-port/28 directory0755/28 simple-token notices; no pristine-output or production configuration approval. Improve each separately when fixture scope changes. |
| M4 startup schema errors | Accepted only finite healthy-after-startup metadata-test scope. Preserve2 ERROR/4 WARN with exact times before healthy wait/test, final all-member status; root cause remains unproven. No waiver for runtime errors or upgrade/restore. Separate production maintenance/restore gates remain mandatory. |

## Root individual Cannot-verify dispositions

| Item | Root decision |
|---|---|
| CV1 exact activated mTLS and interval | Mandatory Task4: independently pinned issuer/runtime/restore/birth and certificate interval; Task3 signature alone grants no peer authority. |
| CV2 one-shot send/recheck | Mandatory Task4: original sealed Prepared/all fences/deadline rechecked at each IO boundary, no side-effect retry. |
| CV3 durable target/clean-state signing | Mandatory Task4: use Task2 durable result; poisoned/accounting-unknown/cold or readable uncertain terminal never signs. |
| CV4 query after closure/expiry | Mandatory Task4: diagnostic authenticated original receipt query with fresh runtime cert, no close redispatch. |
| CV5 CloseAll | Mandatory downstream separate protocol; current receipt only data admission. |
| CV6 launcher registry/Wait drain | Mandatory downstream; retain all preaccepted command monitors/sole Wait ownership. |
| CV7 FUSE/data drain | Mandatory downstream independently from launcher drain. |
| CV8 remote settlement | Mandatory downstream settlement or explicit external write fence before safe reuse; native metadata cannot prove it. |
| CV9 final sync/flush | Mandatory downstream exact-runtime data completion evidence. |
| CV10 physical termination/End | Mandatory downstream exact incarnation termination, no synthetic End from closure. |
| CV11 safe owner/control/index release | Mandatory downstream conditional release with both sender isolation and remote evidence. |
| CV12 production cutover/Redis removal | Mandatory remaining master plan; current production still Redis, no migration-complete claim. |
| CV13 full accepted Task2 journal invariants | Prior accepted gate remains valid; named integration seam check only, no replay/re-certification by Task3. |
| CV14 accepted Task1 attack matrix | Prior accepted pure-protocol gate remains valid; new context derivation checked, no whole-suite replay. |
| CV15 Linux application race/CGO0/PID1 | Accepted exact evidence limit: Darwin race client plus actual Linux etcd. Task4 actual protected Linux target required; no expanded claim. |
| CV16 large existing-N/long run | Mandatory master capacity gates;91 finite fixtures not fleet benchmark or performance improvement. |
| CV17 actual schema-error root cause | Unproven; preserve independent anomaly tracking, no source-timing inference promoted to proven diagnosis. |
| CV18 upgrade/downgrade | Mandatory relevant deployment lifecycle verification, absent here. |
| CV19 crash/backup/restore | Mandatory production restore-epoch and storage maintenance verification, absent here. |
| CV20 coherent cold-history replacement | Accepted structural API scope: history is snapshot without external anchor; cannot adopt Prepared or authorize write. |
| CV21 inherited Stage/issuer/clock/provider | Prior accepted protocols retained; named coupling and real G registry checks only. Concrete operator provider still must meet immutable/context/concurrency contract. |
| CV22 excluded neighbors/whole integration | Mandatory one unfiltered original-unit review after Task4; Task3 owned scope does not substitute it. |
| CV23 final host-only selector exact bytes | Accepted precise attribution: earlier host final has only test assertion difference; final A actual race/native covers corrected bytes, all product same. No redundant host replay. |

## Gate

Task3 accepted. Root may dispatch Task4 on a newly captured exact BASE after this documentation gate. The current unit and complete Redis migration remain incomplete. All OWN retained; no exhaustive-human-publication or deletion step has been taken. Reviewer reader KeyError/FileNotFoundError and initial truncated historical hex inspection were corrected by complete raw artifact parsing, not hidden as product/test successes.
