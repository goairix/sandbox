# AppArmor HCE source builder contract

This directory defines a fail-closed contract for building AppArmor 4.1.7
parser RPMs for HCE EulerOS 2.0. The builder is intentionally not connected to
any cluster and does not read kubeconfig, credentials, or arbitrary host
configuration.

Usage requires all three explicit options:

```text
./build.sh --arch amd64|arm64 \
  --builder-image registry/path@sha256:<64-hex-digest> \
  --output-dir /absolute/output/directory
```

`--arch` must be exactly `amd64` or `arm64`; it maps to `x86_64` and
`aarch64`, respectively. `--builder-image` must be supplied by an
administrator, be digest-pinned, and identify an image that is actually HCE
2.0 with the matching architecture. There is no implied/default base image.
`--output-dir` must be supplied; it is created if absent.

The source lock uses the AppArmor project's official upstream GitHub release
under the `apparmor/apparmor` organization (the project's canonical source
repository), and records its release URL, signature URL, digest,
trusted key fingerprint, and source revision. The digest, fingerprint, and
revision are currently `UNVERIFIED_*` placeholders because this checkout does
not contain authoritative upstream verification evidence. Build execution is
therefore rejected until an administrator verifies those fields against the
official signed release and records the resulting values. Do not replace them
with guesses.

No network download or build stage is performed by the current validation-only
stub. No secrets may be passed in arguments, environment variables, or output;
do not put tokens, passwords, kubeconfigs, or private registry credentials in
this directory or its logs.

When an administrator enables the Containerfile build, the HCE builder must
provide a read-only GPG keyring containing the authoritative AppArmor release
signing key and pass its path as the `APPARMOR_TRUSTED_KEYRING` build argument.
The build downloads the locked `.asc` signature and verifies its machine-
readable signer fingerprint with that keyring before extracting the archive;
it never imports keys or uses the host's default keyring.

The `trusted_public_key_fingerprint` lock value must be the exact signing-key
fingerprint emitted by GPG's `VALIDSIG` status for the locked release; it is
not an unverified primary-key alias. No key material is stored in this repo.

The early builder gate also requires `/sbin` to be a symlink resolving exactly
to `/usr/sbin`. This HCE usr-merge layout makes the packaged real binary at
`%{_sbindir}/apparmor_parser` reachable as `/sbin/apparmor_parser` without a
wrapper or host mutation.
