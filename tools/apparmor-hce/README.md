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
