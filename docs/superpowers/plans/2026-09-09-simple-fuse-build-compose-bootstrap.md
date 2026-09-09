# Simple FUSE Build and Compose Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make both FUSE images build directly without operator-supplied base image or SHA arguments, and provide one safe Docker Compose initialization command for workspace credentials and local directories.

**Architecture:** Build s3fs 1.95 from its immutable upstream commit inside both FUSE Dockerfiles, compute the installed binary hash inside the image, and keep the existing profile self-check. Extract the ordinary sandbox package installation into one repository script shared by ordinary and Docker FUSE images. Add a fail-closed Compose preparation script that writes file credentials without putting secrets in process environment variables.

**Tech Stack:** Docker BuildKit/buildx, POSIX shell, Bash contract tests, Docker Compose, Go workspace mounter/probe binaries.

---

## File structure

- Create `docker/images/common/build-s3fs.sh`: build exactly s3fs 1.95 commit `561ce1e20600bab42133b5e51511483800d3ef76` into a supplied destination root.
- Create `docker/images/sandbox/install-runtime.sh`: hold the existing Python/Node/system package installation shared by ordinary and Docker FUSE runtime images.
- Modify `docker/images/sandbox/Dockerfile`: call the shared runtime installer while preserving the ordinary runtime contract.
- Modify `docker/images/workspace-mounter/Dockerfile`: add an internal s3fs builder and a concrete Debian runtime stage; remove all external artifact arguments.
- Modify `docker/images/sandbox-fuse/Dockerfile`: build from a concrete Python base, call the shared runtime installer, build s3fs internally, and preserve the supervisor contract.
- Create `docker/prepare.sh`: initialize `.env`, workspace credentials, optional CA, API key, and the FUSE staging root.
- Create `scripts/test-compose-prepare.sh`: exercise safe initialization and failure behavior without touching the checked-in `docker/.env`.
- Modify `scripts/test-fuse-images.sh`: enforce self-contained Dockerfiles and invoke the preparation-script tests.
- Modify `scripts/verify-fuse-image.sh`: retain installed s3fs hash verification and remove the obsolete external base digest assertion.
- Modify deployment documentation and `.env.example`: describe only direct image builds and the single Compose preparation command.

### Task 1: Lock the new image contracts in tests

**Files:**
- Modify: `scripts/test-fuse-images.sh`
- Modify: `scripts/verify-fuse-image.sh`

- [ ] **Step 1: Replace external-input assertions with self-contained assertions**

For each FUSE Dockerfile, require the fixed commit, internal source builder, computed installed-binary hash, and forbid the three former arguments:

```bash
s3fs_commit=561ce1e20600bab42133b5e51511483800d3ef76
grep -Fq "S3FS_COMMIT=$s3fs_commit" "$file"
grep -Fq 'COPY docker/images/common/build-s3fs.sh' "$file"
grep -Fq 'sha256sum /usr/bin/s3fs' "$file"
! grep -Eq '^ARG (BASE_IMAGE|S3FS_PACKAGE_URL|S3FS_PACKAGE_SHA256)(=|$)' "$file"
```

Also require concrete final bases, the shared sandbox installer in ordinary/Docker FUSE images, and keep the negative assertion that the ordinary runtime contains neither s3fs nor `workspace-mounter`.

- [ ] **Step 2: Remove the obsolete base-image digest runtime check**

Delete the `base-image.digest` read and `@sha256:` validation from the shell executed by `verify-fuse-image.sh`. Keep:

```sh
stored_s3fs_sha=$(cat s3fs-package.sha256)
test "$(sha256sum /usr/bin/s3fs | cut -d " " -f 1)" = "$stored_s3fs_sha"
```

- [ ] **Step 3: Run the contract test and verify the intended RED state**

Run: `./scripts/test-fuse-images.sh`

Expected: FAIL because the current Dockerfiles still declare `BASE_IMAGE`, `S3FS_PACKAGE_URL`, and `S3FS_PACKAGE_SHA256`.

- [ ] **Step 4: Commit the failing contract**

```bash
git add scripts/test-fuse-images.sh scripts/verify-fuse-image.sh
git commit -m "test: require self-contained fuse images"
```

### Task 2: Build s3fs internally and share sandbox runtime setup

**Files:**
- Create: `docker/images/common/build-s3fs.sh`
- Create: `docker/images/sandbox/install-runtime.sh`
- Modify: `docker/images/sandbox/Dockerfile`
- Modify: `docker/images/workspace-mounter/Dockerfile`
- Modify: `docker/images/sandbox-fuse/Dockerfile`
- Modify: `docker/images/sandbox/Dockerfile.dockerignore`

- [ ] **Step 1: Add the pinned s3fs source builder**

Implement a POSIX shell script with this contract:

```sh
#!/bin/sh
set -eu
destination=${1:?destination root is required}
commit=561ce1e20600bab42133b5e51511483800d3ef76
git clone --branch v1.95 --depth 1 https://github.com/s3fs-fuse/s3fs-fuse.git /tmp/s3fs
test "$(git -C /tmp/s3fs rev-parse HEAD)" = "$commit"
cd /tmp/s3fs
./autogen.sh
./configure --prefix=/usr
make -j"$(getconf _NPROCESSORS_ONLN)"
make DESTDIR="$destination" install
test -x "$destination/usr/bin/s3fs"
```

The builder stage installs `automake`, `autotools-dev`, `g++`, `git`, `libcurl4-openssl-dev`, `libfuse3-dev`, `libssl-dev`, `libxml2-dev`, `make`, and `pkg-config` before calling the script.

- [ ] **Step 2: Extract the ordinary runtime installation**

Move the existing apt, Node.js, pip, Playwright, Jupyter, npm dependency, UID/GID 1000, and `/workspace` setup commands from `docker/images/sandbox/Dockerfile` into `docker/images/sandbox/install-runtime.sh`. The script must be non-interactive, use `set -eu`, leave `/workspace` owned by `sandbox:sandbox`, and leave the caller responsible for `USER`, `CMD`, and FUSE-specific files.

- [ ] **Step 3: Keep the ordinary runtime behavior unchanged**

Use `python:3.13-slim-bookworm`, copy and run the shared installer, remove it afterward, copy only `workspace-probe`, then retain:

```dockerfile
WORKDIR /workspace
USER sandbox
CMD ["sleep", "infinity"]
```

Update the deny-by-default Dockerfile ignore file to admit only the new shared installer in addition to the existing Go sources and module files.

- [ ] **Step 4: Make the Kubernetes mounter image self-contained**

Use three stages: Go tools builder, Debian s3fs builder with `ENV S3FS_COMMIT=561ce1e20600bab42133b5e51511483800d3ef76`, and `debian:bookworm-slim` runtime. Install `ca-certificates`, `fuse3`, `libcurl4`, `libssl3`, and `libxml2`; copy `/out/usr/bin/s3fs`; compute its SHA and substitute the unique zero placeholder in `profile-bundle.json`:

```dockerfile
RUN s3fs_sha="$(sha256sum /usr/bin/s3fs | cut -d ' ' -f 1)" \
 && sed "s/0000000000000000000000000000000000000000000000000000000000000000/$s3fs_sha/" \
      /tmp/workspace-fuse-profile-bundle.json > /etc/workspace-fuse/profile-bundle.json \
 && printf '%s\n' "$s3fs_sha" > /etc/workspace-fuse/s3fs-package.sha256
```

Remove `base-image.digest`; preserve root-owned directory modes, `user_allow_other`, setuid/setgid stripping, the image self-check, and supervisor entrypoint.

- [ ] **Step 5: Make the Docker FUSE image self-contained**

Use the same Go and s3fs builder contracts, then start from `python:3.13-slim-bookworm`, call `install-runtime.sh`, install the FUSE runtime libraries, copy s3fs plus both Go tools, compute the binary SHA into the bundle, and preserve `WORKDIR /workspace` plus the supervisor entrypoint. Do not switch the final image to `USER sandbox`; the supervisor starts as root and launches user execution under UID/GID 1000.

- [ ] **Step 6: Run static and BuildKit checks**

Run:

```bash
./scripts/test-fuse-images.sh
docker buildx build --check -f docker/images/workspace-mounter/Dockerfile .
docker buildx build --check -f docker/images/sandbox-fuse/Dockerfile .
docker buildx build --check -f docker/images/sandbox/Dockerfile .
```

Expected: contract tests print `fuse image contract tests: PASS`; each BuildKit check exits 0 with no undefined-argument warning.

- [ ] **Step 7: Commit the image implementation**

```bash
git add docker/images scripts/test-fuse-images.sh scripts/verify-fuse-image.sh
git commit -m "build: make fuse images self-contained"
```

### Task 3: Add safe one-command Compose preparation

**Files:**
- Create: `docker/prepare.sh`
- Create: `scripts/test-compose-prepare.sh`
- Modify: `scripts/test-fuse-images.sh`
- Modify: `docker/.env.example`

- [ ] **Step 1: Write preparation-script behavior tests**

The test creates a temporary `.env`, input credential files, credentials directory, staging directory, and a fake `docker` executable. Assert:

```bash
test "$(cat "$credentials/accessKey")" = test-access
test "$(cat "$credentials/secretKey")" = test-secret
test "$(file_mode "$credentials")" = 700
test "$(file_mode "$credentials/accessKey")" = 400
test "$(file_mode "$credentials/secretKey")" = 400
grep -Eq '^SECURITY_API_KEY=[A-Za-z0-9_-]{32,}$' "$env_file"
```

Also assert that an existing non-default API key is preserved, `.env` is not overwritten, an invalid CA fails, missing non-interactive credential files fail, secrets do not appear in stdout/stderr, and a second run succeeds without changing file contents.

- [ ] **Step 2: Run the new test and verify the RED state**

Run: `./scripts/test-compose-prepare.sh`

Expected: FAIL because `docker/prepare.sh` does not exist.

- [ ] **Step 3: Implement argument parsing and secret input**

Support these non-secret path options:

```text
--env-file PATH
--credential-dir PATH
--access-key-file PATH
--secret-key-file PATH
--ca-file PATH
--staging-root PATH
--non-interactive
```

When file options are absent in interactive mode, use `read` for AK and `read -s` for SK. Reject newline-containing or empty credentials. Never accept AK/SK values as command-line options and never echo their contents.

- [ ] **Step 4: Implement atomic files, `.env`, and staging preparation**

Create temporary files in the destination directory, set mode `0400`, and rename them over final credential files. Copy `.env.example` only when the selected `.env` does not exist. Replace only `SECURITY_API_KEY=`, `WORKSPACE_CREDENTIAL_DIR=`, and `WORKSPACE_SECRET_STAGING_ROOT=` using a temporary file and atomic rename. Generate the API key with `openssl rand -base64 36` converted to URL-safe characters.

Resolve staging to a canonical absolute path. If FUSE is enabled, create it with `install -d -o root -g root -m 0700`, invoking `sudo` only when the current process is not root, then verify owner and mode with Linux `stat`. Sync-only preparation does not require a staging directory.

- [ ] **Step 5: Validate CA and Compose configuration**

Accept a CA file only if it is non-empty and contains both `-----BEGIN CERTIFICATE-----` and `-----END CERTIFICATE-----`; copy it atomically as mode `0400`. Validate Docker availability and run:

```bash
docker compose --env-file "$env_file" \
  -f "$repo_root/docker/docker-compose.yml" config >/dev/null
```

Exit nonzero before any service startup on all validation errors. On success print the exact `docker compose ... up -d` command, without credentials.

- [ ] **Step 6: Run preparation tests and the existing image contract**

Run:

```bash
./scripts/test-compose-prepare.sh
./scripts/test-fuse-images.sh
```

Expected: both exit 0 and print their PASS markers.

- [ ] **Step 7: Commit the Compose preparation workflow**

```bash
git add docker/prepare.sh docker/.env.example scripts/test-compose-prepare.sh scripts/test-fuse-images.sh
git commit -m "deploy: add compose preparation command"
```

### Task 4: Replace complex operator instructions

**Files:**
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Modify: `docs/deployment/docker-compose-deployment-upgrade.md`
- Modify: `docs/deployment/workspace-fuse.md`
- Modify: `docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md`

- [ ] **Step 1: Simplify the Helm/image release section**

Replace the FUSE build-argument block with the two direct commands from the approved design. Remove the limitation claiming that the repository lacks FUSE base images/artifact definitions. Retain version-tag update, manifest, SBOM, scan, signing, attestation, and release-check guidance.

- [ ] **Step 2: Make Compose initialization the primary installation path**

Explain the three workspace credential files in one table, state that `ca.crt` comes from the endpoint certificate issuer and is normally absent, then make `./docker/prepare.sh` the only first-install preparation command. Keep release drain requirements only for stateful/backend-changing upgrades; show ordinary image-only upgrades as one `docker compose up -d`.

- [ ] **Step 3: Align the focused FUSE runbook and main design**

Remove operator-facing base digest/artifact SHA requirements and point to the self-contained build section. Preserve the existing rule that TLS verification cannot be disabled and the network rule “开放公网、禁止内网、内网仅显式白名单”.

- [ ] **Step 4: Verify documentation has no dangling build inputs**

Run:

```bash
if rg -n '\$(MOUNTER_BASE_IMAGE|DOCKER_FUSE_BASE_IMAGE|S3FS_PACKAGE_URL|S3FS_PACKAGE_SHA256|GO_BUILDER_IMAGE|SANDBOX_BASE_IMAGE)' docs/deployment; then
  exit 1
fi
git diff --check
```

Expected: no variable matches and `git diff --check` exits 0.

- [ ] **Step 5: Commit the runbook update**

```bash
git add docs/deployment docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md
git commit -m "docs: simplify fuse build and compose setup"
```

### Task 5: Full regression verification

**Files:**
- Verify only.

- [ ] **Step 1: Run Go verification**

```bash
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./internal/config ./internal/runtime/docker ./internal/runtime/kubernetes
```

Expected: all packages pass with zero failures.

- [ ] **Step 2: Run deployment and image contracts**

```bash
./scripts/test-compose-prepare.sh
./scripts/test-fuse-images.sh
./scripts/test-workspace-fuse-matrix.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
git diff --check
```

Expected: every script exits 0, Compose renders, and the worktree has no uncommitted implementation changes.

- [ ] **Step 3: Build the self-contained images when registry/network access is available**

```bash
docker buildx build --platform linux/arm64 \
  -f docker/images/workspace-mounter/Dockerfile \
  -t sandbox-fuse-mounter:verify --load .
docker buildx build --platform linux/arm64 \
  -f docker/images/sandbox-fuse/Dockerfile \
  -t sandbox-fuse-docker:verify --load .
FUSE_IMAGE=sandbox-fuse-mounter:verify \
SANDBOX_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-runtime:v0.2.12 \
  ./scripts/verify-fuse-image.sh kubernetes package-check
SANDBOX_IMAGE=sandbox-fuse-docker:verify \
  ./scripts/verify-fuse-image.sh docker package-check
```

Expected: both builds and package checks exit 0. If an external registry or upstream source is unreachable, record the exact failing fetch separately; do not report the image build as verified.

- [ ] **Step 4: Review repository state**

Run: `git status --short --branch && git log --oneline -8`

Expected: the feature branch contains the scoped commits above and no untracked secret or generated credential file.
