# Kubernetes Shared Ordinary Pool Design

## Problem

The ordinary sandbox pool is process-local. In a multi-replica Kubernetes
deployment, every API replica keeps a private slice of warm Pod identities,
while startup cleanup deletes every non-session pool Pod it can see. A replica
therefore deletes Pods still referenced by another replica. The stale identity
is only discovered when a later request reaches that replica, which can create
a new Pod on demand even though older warm Pods are running in the namespace.

## Required behavior

- Ordinary warm Pods form one release-scoped inventory shared by all API
  replicas.
- Claiming a prepared Pod is atomic; one Pod can serve at most one sandbox.
- A claimed Pod remains single-use and is destroyed on sandbox release.
- Pool minimum and maximum sizes are release-wide, not per replica.
- Restarting or rolling one API replica must neither delete nor duplicate the
  shared inventory.
- A release drain still removes all ordinary pool Pods and durable inventory.
- Docker and tests without a shared store retain the existing local pool.

## Design

For Kubernetes, the existing Redis `state.AtomicStore` is used as the durable
inventory. Each record contains a preparation ID, immutable runtime identity,
pool fingerprint, state, update time, claim deadline, and revision. Records are
scoped by runtime namespace and a fingerprint of the effective ordinary Pod
spec so separate releases and incompatible configurations cannot share Pods.

The record lifecycle is:

1. `preparing`: admitted under a release-scoped refill lock before Pod creation.
2. `prepared`: the Pod is running and available to every replica.
3. `claimed`: acquired by CAS with a short deadline while the pool labels are
   removed from the Pod. The record is then deleted; the Pod belongs to the
   sandbox session and is no longer pool inventory.

Acquisition scans prepared records and CAS-claims one, then verifies runtime
ID, UID, and running state. The Manager uses the Kubernetes runtime's existing
policy-first identity migration to remove pool ownership and assign the public
sandbox ID atomically; only then is the durable claim retired. If no valid
record exists, it creates an on-demand single-use Pod and schedules a shared
refill.

Refill is serialized with a renewable Redis lock. The lock holder reconciles
records and Pods, counts global preparing/prepared inventory, and creates only
the missing capacity up to the configured bounds.

Startup reconciliation runs after session restoration. It repairs an
interrupted preparation when record and Pod identity agree, removes stale
current-fingerprint records, and adopts an unrecorded prepared Pod carrying the
current preparation ID. Pods with another fingerprint, including legacy Pods
without one, are left for their overlapping old replica or the release drain;
this makes the protocol safe to introduce and to roll between image versions.
It never treats another API replica's prepared Pod as an orphan.

Every serving replica maintains a renewable owner key for its pool fingerprint.
Normal manager shutdown stops local refill work and unregisters that owner; the
last owner destroys that fingerprint's warm inventory. This keeps a same-version
rolling restart shared while retiring the old image inventory after a version
rollout. Release drain separately destroys every fingerprint and deletes all
ordinary-pool state.

## Failure rules

- Store or reconciliation failures are fatal during Kubernetes manager start;
  silently falling back to a local pool would recreate the original defect.
- A CAS loss means another replica won and the caller tries another record.
- Runtime UID mismatch invalidates the record but is not deletion authority for
  an object with a different UID.
- A crash after claim but before pool-label removal leaves a time-bounded
  `claimed` record. Reconciliation removes that still-pooled runtime only after
  the claim expires.
- A crash after label removal cannot cause re-adoption because the Pod no longer
  matches pool inventory.

## Verification

Tests cover cross-replica reuse, concurrent single-winner claims, global refill
cardinality, startup reconciliation, normal shutdown preservation, and release
drain cleanup. The deployed verification must pin requests to individual API
replicas and prove a replica can acquire a Pod created before that replica's
request without creating a short-lived replacement.
