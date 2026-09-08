# Simplify Container Image Builds Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore direct per-image `docker buildx build` releases, use explicit semantic version tags such as `v0.2.12`, and remove host-side FUSE binary/build-context assembly.

**Architecture:** FUSE Dockerfiles gain Go builder stages and consume the repository root context, so each image is produced by one Docker build. A small shared Go image-reference validator and equivalent shell checks accept strict release tags or legacy sha256 digests while rejecting mutable tags such as `latest`. Helm and Compose remain deployment tools: operators update versioned image references and run the normal upgrade command.

**Tech Stack:** Go 1.25, Docker Buildx/Dockerfile multi-stage builds, Docker Compose, Helm, Bash contract tests.

---

## File map

- Create `internal/imageref/release.go`: shared Go validation for project release image references.
- Create `internal/imageref/release_test.go`: table tests for semantic version tags and legacy digests.
- Modify `internal/config/config.go` and `internal/config/config_test.go`: allow versioned FUSE image configuration.
- Modify `internal/runtime/docker/container.go`, `internal/runtime/docker/network.go`, and Docker runtime tests: apply the shared validator to FUSE and gateway images.
- Modify `internal/runtime/kubernetes/pod.go` and Kubernetes runtime tests: apply the shared validator to ordinary runtime and mounter images used by FUSE Pods.
- Modify `scripts/verify-fuse-image.sh`, `scripts/workspace-fuse-preflight.sh`, `scripts/workspace-fuse-matrix.sh`, and their tests: mirror the version-tag-or-digest contract in release tooling.
- Modify `docker/images/workspace-mounter/Dockerfile` and `docker/images/sandbox-fuse/Dockerfile`: compile required Go binaries in Docker builder stages.
- Modify `scripts/test-fuse-images.sh`: assert self-contained root-context FUSE builds.
- Modify `docker/docker-compose.yml` and `docker/.env.example`: make the API and runtime image references explicit production inputs.
- Modify `deploy/helm/sandbox/values.yaml`, `testdata/values-fuse-*.yaml`, and `scripts/test-helm-chart.sh`: use the version-tagged release naming convention.
- Modify `docs/deployment/helm-deployment-upgrade.md`, `docs/deployment/docker-compose-deployment-upgrade.md`, and `docs/deployment/workspace-fuse.md`: document direct builds and simple image-reference updates.

### Task 1: Shared release image reference validation

**Files:**
- Create: `internal/imageref/release.go`
- Create: `internal/imageref/release_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/runtime/docker/container.go`
- Modify: `internal/runtime/docker/network.go`
- Modify: `internal/runtime/docker/container_test.go`
- Modify: `internal/runtime/docker/runtime_test.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/runtime/kubernetes/pod_test.go`

- [ ] **Step 1: Add failing validator tests**

Create table tests requiring these results:

```go
func TestIsRelease(t *testing.T) {
    digest := "registry.example.com/sandbox@sha256:" + strings.Repeat("a", 64)
    tests := []struct {
        image string
        want  bool
    }{
        {"registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.2.12", true},
        {"registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.2.12-rc.1", true},
        {digest, true},
        {"registry.example.com/sandbox:latest", false},
        {"registry.example.com/sandbox", false},
        {"registry.example.com/sandbox:arm64", false},
        {"registry.example.com/sandbox:0.2.12", false},
        {"registry.example.com/sandbox:v01.2.3", false},
    }
    for _, tt := range tests {
        assert.Equal(t, tt.want, imageref.IsRelease(tt.image), tt.image)
    }
}
```

- [ ] **Step 2: Run the new package test and confirm RED**

Run: `go test ./internal/imageref -count=1`

Expected: FAIL because `imageref.IsRelease` does not exist.

- [ ] **Step 3: Implement the shared validator**

Use `github.com/distribution/reference` to parse a normalized named reference. Return true for a valid lowercase sha256 digest or for a tagged reference whose tag matches strict Docker-compatible SemVer:

```go
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

func IsRelease(image string) bool {
    named, err := reference.ParseNormalizedNamed(image)
    if err != nil {
        return false
    }
    if digested, ok := named.(reference.Digested); ok {
        digest := digested.Digest()
        encoded := digest.Encoded()
        return digest.Algorithm() == "sha256" && len(encoded) == 64 &&
            encoded == strings.ToLower(encoded) && isLowerHex(encoded)
    }
    tagged, ok := named.(reference.Tagged)
    return ok && releaseTag.MatchString(tagged.Tag())
}
```

- [ ] **Step 4: Run validator tests and confirm GREEN**

Run: `go test ./internal/imageref -count=1`

Expected: PASS.

- [ ] **Step 5: Replace duplicate digest-only runtime checks**

Import `internal/imageref` in config, Docker runtime, and Kubernetes runtime. Replace digest-only predicates for FUSE mounter, Docker FUSE runtime, Kubernetes sandbox image, and Docker gateway with `imageref.IsRelease`. Update errors to say `must use a vMAJOR.MINOR.PATCH tag or valid sha256 digest`.

Keep digest support; do not weaken unrelated immutable runtime UID, Pool identity, credential, or storage validation.

- [ ] **Step 6: Add version-tag cases to each caller test**

Add positive tests using `registry.example.com/...:v0.2.12`, retain positive digest cases, and retain negative `latest` cases in config, Docker container/gateway, and Kubernetes Pod validation tests.

- [ ] **Step 7: Run caller tests**

Run:

```bash
go test ./internal/config ./internal/runtime/docker ./internal/runtime/kubernetes -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/imageref internal/config internal/runtime/docker internal/runtime/kubernetes
git commit -m "feat: allow version-tagged release images"
```

### Task 2: Release shell tooling accepts version tags

**Files:**
- Modify: `scripts/verify-fuse-image.sh`
- Modify: `scripts/workspace-fuse-preflight.sh`
- Modify: `scripts/workspace-fuse-matrix.sh`
- Modify: `scripts/test-fuse-images.sh`
- Modify: `scripts/test-workspace-fuse-preflight.sh`
- Modify: `scripts/test-workspace-fuse-matrix.sh`

- [ ] **Step 1: Add failing shell test cases**

Add positive cases for `registry.example.com/sandbox:v0.2.12` and `registry.example.com/sandbox:v0.2.12-rc.1`. Add negative cases for `latest`, an untagged reference, and `arm64`. Preserve all digest cases.

- [ ] **Step 2: Run the shell tests and confirm RED**

Run:

```bash
./scripts/test-fuse-images.sh
./scripts/test-workspace-fuse-preflight.sh
./scripts/test-workspace-fuse-matrix.sh
```

Expected: at least one new version-tag case fails at the digest-only guard.

- [ ] **Step 3: Replace digest-only shell guards**

Implement one equivalent Bash predicate in each standalone script:

```bash
require_release_image() {
  [[ "$2" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ||
     "$2" =~ ^[^[:space:]@]+:v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]] \
    || fail "$1 must use a vMAJOR.MINOR.PATCH tag or valid sha256 digest"
}
```

Use the predicate for mounter, Docker FUSE, ordinary sandbox, and gateway release references. Keep evidence SHA256 validation unchanged; it verifies evidence content, not the image naming convention.

- [ ] **Step 4: Run shell tests and confirm GREEN**

Run the three commands from Step 2.

Expected: all print their PASS/completion messages.

- [ ] **Step 5: Commit**

```bash
git add scripts/verify-fuse-image.sh scripts/workspace-fuse-preflight.sh scripts/workspace-fuse-matrix.sh scripts/test-fuse-images.sh scripts/test-workspace-fuse-preflight.sh scripts/test-workspace-fuse-matrix.sh
git commit -m "build: accept versioned images in fuse checks"
```

### Task 3: Make FUSE Dockerfiles self-contained

**Files:**
- Modify: `docker/images/workspace-mounter/Dockerfile`
- Modify: `docker/images/sandbox-fuse/Dockerfile`
- Modify: `scripts/test-fuse-images.sh`

- [ ] **Step 1: Add failing static Dockerfile contracts**

Require both FUSE Dockerfiles to declare a `GO_BUILDER_IMAGE`, create a `workspace-tools-builder` stage, copy only required Go modules/packages, and install binaries from that stage. Require the mounter image to build only `workspace-mounter`; require the Docker FUSE image to build both binaries. Reject plain `COPY workspace-mounter` and `COPY workspace-probe` from a generated host context.

- [ ] **Step 2: Run the image contract test and confirm RED**

Run: `./scripts/test-fuse-images.sh`

Expected: FAIL because both Dockerfiles still copy host-built binaries.

- [ ] **Step 3: Add the mounter builder stage**

At the start of `docker/images/workspace-mounter/Dockerfile`, add:

```dockerfile
ARG GO_BUILDER_IMAGE=golang:1.25-alpine
FROM ${GO_BUILDER_IMAGE} AS workspace-tools-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/workspace-mounter ./cmd/workspace-mounter
COPY internal/fuseprotocol ./internal/fuseprotocol
COPY internal/mounter ./internal/mounter
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/workspace-mounter ./cmd/workspace-mounter
```

In the final stage, copy `docker/images/workspace-mounter/profile-bundle.json` from the root context and copy `/out/workspace-mounter` from `workspace-tools-builder`.

- [ ] **Step 4: Add the Docker FUSE builder stage**

Use the same module inputs plus `cmd/workspace-probe`, `internal/workspaceprobe`, and build both binaries into `/out`. Copy both from the builder stage in the final image. Copy the same fixed profile bundle path from the root context.

- [ ] **Step 5: Run Dockerfile contract and Go build tests**

Run:

```bash
./scripts/test-fuse-images.sh
go build ./cmd/workspace-mounter ./cmd/workspace-probe
```

Expected: PASS with no generated repository files.

- [ ] **Step 6: Commit**

```bash
git add docker/images/workspace-mounter/Dockerfile docker/images/sandbox-fuse/Dockerfile scripts/test-fuse-images.sh
git commit -m "build: compile fuse tools inside images"
```

### Task 4: Make deployment image inputs explicit and versioned

**Files:**
- Modify: `docker/docker-compose.yml`
- Modify: `docker/.env.example`
- Modify: `deploy/helm/sandbox/values.yaml`
- Modify: `testdata/values-fuse-minio.yaml`
- Modify: `testdata/values-fuse-obs-private.yaml`
- Modify: `testdata/values-fuse-obs-public.yaml`
- Modify: `scripts/test-helm-chart.sh`
- Modify: `scripts/test-fuse-images.sh`

- [ ] **Step 1: Add failing config assertions**

Assert Compose maps `SANDBOX_API_IMAGE` to `sandbox-api.image`, and the production example uses the five `registry.i.huaxisy.com/library/ai-infra/...:v0.2.12` references. Assert the three Helm fixture files use `v0.2.12` without `@sha256` or architecture suffixes.

- [ ] **Step 2: Run config tests and confirm RED**

Run:

```bash
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
```

Expected: FAIL on the new versioned image assertions.

- [ ] **Step 3: Update Compose image inputs**

Set the API image to `${SANDBOX_API_IMAGE:-sandbox-api:latest}`. In `.env.example`, add:

```dotenv
SANDBOX_API_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.2.12
SANDBOX_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-runtime:v0.2.12
GATEWAY_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-gateway:v0.2.12
FUSE_MOUNTER_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter:v0.2.12
FUSE_SANDBOX_IMAGE=registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-docker:v0.2.12
```

Keep the local fallback builds for blank runtime/gateway variables. Production with configured images uses `docker compose up -d -t 600` without `--build`; local development may use `--build`.

- [ ] **Step 4: Update Helm release fixtures and examples**

Use repository `registry.i.huaxisy.com/library/ai-infra/sandbox-api` plus tag `v0.2.12` for the API. Use full `:v0.2.12` references for ordinary runtime, gateway, mounter, and Docker FUSE fields. Do not change the approved private Redis override in test fixtures.

- [ ] **Step 5: Run Compose and Helm tests**

Run:

```bash
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add docker/docker-compose.yml docker/.env.example deploy/helm/sandbox/values.yaml testdata/values-fuse-minio.yaml testdata/values-fuse-obs-private.yaml testdata/values-fuse-obs-public.yaml scripts/test-helm-chart.sh scripts/test-fuse-images.sh
git commit -m "deploy: use versioned project images"
```

### Task 5: Simplify build and upgrade documentation

**Files:**
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Modify: `docs/deployment/docker-compose-deployment-upgrade.md`
- Modify: `docs/deployment/workspace-fuse.md`

- [ ] **Step 1: Replace the Helm build section**

Set `VERSION=v0.2.12` and `REGISTRY=registry.i.huaxisy.com/library/ai-infra`. Provide one root-context `docker buildx build` command for API, ordinary runtime, mounter, and Docker FUSE, plus one gateway-directory command. Remove host `go build`, `mktemp`, `cp`, generated build contexts, Git-derived tags, architecture suffixes, mandatory project-image digest lookup, and digest回填 instructions.

State that operators use their existing `docker manifest` process for multi-architecture publication.

- [ ] **Step 2: Simplify Helm deployment updates**

Document only these deployment actions after publishing images:

```yaml
image:
  repository: registry.i.huaxisy.com/library/ai-infra/sandbox-api
  tag: v0.2.12
config:
  images:
    sandbox: registry.i.huaxisy.com/library/ai-infra/sandbox-runtime:v0.2.12
    gateway: registry.i.huaxisy.com/library/ai-infra/sandbox-gateway:v0.2.12
  workspace:
    fuseImages:
      mounter: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter:v0.2.12
      docker: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-docker:v0.2.12
```

Then run the existing `helm upgrade --install` command. Keep backend drain guidance separate from image build guidance.

- [ ] **Step 3: Simplify Docker Compose deployment updates**

Document updating the five versioned image variables and running:

```bash
docker compose --env-file docker/.env -f docker/docker-compose.yml \
  up -d -t 600
```

Keep `--build` only in a clearly labeled local-source-development example. Remove production instructions that separately build API and invoke `sandbox-images` manually.

- [ ] **Step 4: Check documentation consistency**

Run:

```bash
rg -n 'git rev-parse|VERSION.*ARCH|BUILD_DIR|mktemp|imagetools inspect|必须使用.*sha256|digest-pinned.*FUSE' docs/deployment
git diff --check
```

Expected: no obsolete build workflow remains; any remaining sha256 text applies only to base images, artifact/evidence hashes, or legacy compatibility.

- [ ] **Step 5: Commit**

```bash
git add docs/deployment/helm-deployment-upgrade.md docs/deployment/docker-compose-deployment-upgrade.md docs/deployment/workspace-fuse.md
git commit -m "docs: simplify image build and deployment"
```

### Task 6: Full verification

**Files:**
- Verify all files changed in Tasks 1-5.

- [ ] **Step 1: Run Go verification**

```bash
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./internal/config ./internal/runtime/docker ./internal/runtime/kubernetes
```

Expected: all commands exit 0.

- [ ] **Step 2: Run deployment and image contracts**

```bash
./scripts/test-fuse-images.sh
./scripts/test-workspace-fuse-preflight.sh
./scripts/test-workspace-fuse-matrix.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
git diff --check
```

Expected: all commands exit 0 and print their normal PASS/completion messages.

- [ ] **Step 3: Inspect final branch state**

```bash
git status --short --branch
git log --oneline -8
```

Expected: clean worktree on `feat/workspace-fuse-mount`; commits from Tasks 1-5 are visible.
