# AppArmor 4.1.7 for HCE 2.0

`build.sh` builds a parser RPM and source RPM, audits them in isolated HCE
userspace, and exports local artifacts. It never pushes an image, creates a
global buildx builder, registers an emulator, or changes a cluster/node.

The workflow implementation has local structural/crypto/CLI tests. **It has
not yet been executed against an administrator-supplied HCE builder.** Package
availability, both real architecture builds, RPM integration, and native HCE
acceptance remain environment-dependent validation gates.

## Inputs and invocation

Host requirements: Bash, Python 3.8+, GnuPG, Helm 3, Docker with an existing buildx
builder. Supply a real HCE 2.0 image pinned by digest, with DNF repositories
configured for its architecture. No builder image is guessed or provided here.

```sh
./tools/apparmor-hce/build.sh \
  --arch amd64 \
  --builder-image registry.example/hce@sha256:<actual-64-hex-digest> \
  --trusted-keyring /absolute/path/to/trusted-public-keyring.gpg \
  --output-dir /existing/parent/apparmor-amd64
```

Use `--arch arm64` separately for AArch64. `--source-lock /absolute/source.lock`
is optional; the checked-in lock is the default. A keyring may be a public
binary export or an ASCII armored public export. It must contain exactly the
locked primary key and its subkeys, and no secret keys. Obtain and authenticate
it independently; the workflow never fetches keys automatically.

The output directory must be absent or empty, its parent must already exist,
and its path must be absolute without commas/control characters. Lock and
keyring validation happens before Docker, network calls, or output-directory
creation. Build context files come from the exact `artifact_set.INPUTS`
allowlist, including only the public workspace profile and its existing Chart
helper/loader templates. No repository-wide context, private values, credentials,
user kubeconfig, or trusted keyring is copied into it. The keyring enters through a BuildKit
secret. Its validated **public** contents are intentionally retained in the
export as source provenance.

Export is staged in a task-owned temporary sibling directory. The workflow
publishes by rename only after all stages and host checks succeed; a nonempty
user output is never replaced. Failed builds clean their own temporary
directories. Docker diagnostics are kept in that private temporary directory
and removed on cleanup; raw arguments, environment values, and registry logs
are not echoed on failure.

## Source trust

The lock pins the official GitLab `v4.1.7` source archive and its detached
signature, not a guessed GitHub release URL:

- [Source archive](https://gitlab.com/apparmor/apparmor/-/archive/v4.1.7/apparmor-v4.1.7.tar.gz)
- [Detached signature](https://gitlab.com/api/v4/projects/4484878/packages/generic/signatures/4.1.7/apparmor-v4.1.7.tar.gz.asc)
- SHA256: `ded4cd419b8a05002a108a0912208e2098695752664c6a59464d2dda418e0452`
- Primary and signing fingerprints: `3ECDCBA5FB34D254961CC53F6689E64E3D3664BB`

These archive values were checked against the detached signature in an isolated
GPG home. The verifier compares the complete `VALIDSIG` signing fingerprint and
primary fingerprint separately, also supporting a distinct signing subkey.
Downloads have HTTPS-only redirects and bounded time, size, and retry limits.
Archive paths, duplicates, special files, and link traversal are checked before
RPM extraction.

`source_revision` records the tag's reported commit as metadata. This pipeline
does not verify a signed Git tag and explicitly requires
`source_revision_verification=unverified`. Archive trust is the pinned hash plus
detached signature; `git ls-remote` is not used as cryptographic evidence.

## Build and audit

The spec follows the release README: libapparmor `autogen.sh`, `configure`,
build, `make check`, then parser build and `make check`. Build dependencies
include the C++ compiler/static C++ library, autoconf-archive, DejaGNU, Perl test
and POD tooling. The upstream parser statically links libapparmor; the RPM
contains only the parser and GPL/LGPL license files. `rpmbuild -ba` produces both
RPMs. No upstream full install target is used.

Before installation, the audit checks RPM identity/architecture, all scriptlet
and trigger families, actual CPIO entries against RPM header entries, exact
paths, modes, owners, capabilities, duplicates, links, source-RPM input bytes,
and RPM digests. ELF checks inspect the complete ELF64/program-table structure,
machine, entry point, interpreter, and segments. `readelf` records NEEDED,
dynamic symbols, and version requirements; unexpected private libapparmor
dependencies and RPATH/RUNPATH are rejected.

A fresh stage starts from the same pinned HCE base, resolves actual RPM
requirements using its repositories, then performs `rpm --test -i`, `rpm -i`,
and `rpm -V`. No dependency/signature/force bypass flags are used. Installed
parser bytes must match the audited RPM. That actual installed binary runs
`--version` and compiles the **complete Helm-generated workspace profile** using
`-Q -K --Werror --warn=all` with no kernel load or cache access. Both commands
record stdout/stderr hashes and exit status. Compile stdout or stderr, nonzero
exit status, timeout, or missing/empty output fails the gate. Reverification
requires a freshly generated binary; stale output is never accepted.

Before Docker, Helm renders a narrow temporary chart using the existing
`sandbox.apparmor.digest` and `sandbox.effectiveLSMProfile` helpers and the
loader's exact replacement expression. It receives a fixed enabled flag and an
empty task-owned kubeconfig; it reads no values files, user kubeconfig, Helm
plugins, or cluster state. The expected normalization is CRLF to LF, trim, then
one final LF; the pre-substitution SHA256 determines `sandbox-fuse-<digest>`.
The renderer checks this binding, retains hashes of the three original Chart
inputs and full rendered policy, and refuses changed loader rendering rules.
Host verification also compares with the current complete Chart policy, so
removed FUSE/mount constraints or extra wildcard grants fail equality checks.

Pure userspace compilation uses exactly
`apparmor-v4.1.7/parser/tst/features_files/features.all` from the already verified
source archive via `--kernel-features`. Its complete bytes, member path, SHA256,
and source-archive hash are retained and rechecked. This is explicitly an
**upstream test fixture**, not a target kernel snapshot or kernel-compatibility
claim. The policy ABI is never overridden and warnings are never suppressed.
`parser_checks.compile_profile` remains the Task 4 hook for an explicitly
validated target-kernel snapshot; target origin/architecture/hash gates remain
separate from this userspace test.

`uname` and target-platform agreement establish the execution architecture,
**not native execution**. BuildKit may be emulating it. Every report explicitly
records execution mode as unknown and `native_live_hce_verified=false`.

## Reverification and artifacts

```sh
./tools/apparmor-hce/verify.sh \
  --arch amd64 \
  --builder-image registry.example/hce@sha256:<actual-64-hex-digest> \
  --trusted-keyring /absolute/path/to/trusted-public-keyring.gpg \
  --output-dir /existing/parent/apparmor-amd64
```

Default verification repeats the RPM transaction and parser checks in the
specified HCE builder and leaves the supplied artifacts unchanged. Add
`--structural-only` to check inventory, complete checksums, source trust, and ELF
structure on the host without Docker; this explicitly does not repeat HCE
runtime or RPM checks.

The exact export set includes the binary RPM, source RPM, parser, source archive,
signature, public key, lock, all build/audit inputs, tool/package versions,
build/test log, installed dependency providers, ELF ABI inventory, parser
version, full rendered profile, compiled policy, feature fixture/provenance,
compile diagnostics/report, provenance, and audit report. `SHA256SUMS` covers every other required
file, including the reports and input scripts. Missing, additional, linked,
duplicated-manifest, or modified artifacts fail verification. Hashes bind this
inventory; they are not an artifact-signing scheme.

Artifacts are explicitly **unsigned testing artifacts**. Ordinary RPM may
accept an unsigned package after verifying its digests; that does not meet
production package-signature policy. Before production use, a separate trusted
RPM signing and signature-verification gate is required. No production-valid
claim is made by this workflow.

Run local tests with `tools/apparmor-hce/tests/contract.sh`. They exercise the
real validators and CLI, ephemeral GPG fixtures, synthetic ELF/CPIO structure,
real Helm equivalence with the production Chart, and substituted Docker/parser
boundaries for orchestration and strict diagnostic handling. They do not establish a
successful HCE build, runtime compatibility, native execution, or real profile
acceptance.
