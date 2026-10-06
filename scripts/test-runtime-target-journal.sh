#!/usr/bin/env bash
# Linux UID0 native filesystem fixture. Controller owns invocation/lifecycle.
# Uses an already-local pinned image; never pulls images or runs etcd.
set -euo pipefail

readonly image='gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
readonly fixture='runtime-target-journal'
readonly project="sandbox-target-journal-test-$$-$(date +%s)"
readonly container="${project}-runner"
readonly volume="${project}-tmp"
scratch=''
container_created=false
volume_created=false

cleanup() {
  local result=$?
  trap - EXIT INT TERM
  # Only remove exact resources whose ownership labels still match this run.
  if [[ "$container_created" == true ]] &&
    [[ "$(docker container inspect --format '{{ index .Config.Labels "sandbox.test.project" }} {{ index .Config.Labels "sandbox.test.fixture" }}' "$container" 2>/dev/null)" == "$project $fixture" ]]; then
    docker container rm -f "$container" >/dev/null || true
  fi
  if [[ "$volume_created" == true ]] &&
    [[ "$(docker volume inspect --format '{{ index .Labels "sandbox.test.project" }} {{ index .Labels "sandbox.test.fixture" }}' "$volume" 2>/dev/null)" == "$project $fixture" ]]; then
    docker volume rm "$volume" >/dev/null || true
  fi
  if [[ -n "$scratch" ]]; then
    rm -rf -- "$scratch"
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

repository="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd -- "$repository"
# Architecture belongs to the inspected Docker image, not the host shell.
image_os="$(docker image inspect --format '{{.Os}}' "$image")"
architecture="$(docker image inspect --format '{{.Architecture}}' "$image")"
if [[ "$image_os" != linux ]]; then
  echo "Pinned fixture image must be Linux, got $image_os" >&2
  exit 2
fi
case "$architecture" in
  arm64|amd64) ;;
  *) echo "Unsupported inspected fixture architecture: $architecture" >&2; exit 2 ;;
esac

scratch="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-target-journal-test.XXXXXX")"
readonly binary="$scratch/journal-test"
readonly output="$scratch/test-output.log"
echo "Fixture project=$project image=$image architecture=$architecture UID=0 network=none caps=CHOWN"
echo 'Compiling CGO_ENABLED=0 GOOS=linux native test binary (-c); this is not test execution or Linux race.'
CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go test -c -o "$binary" ./internal/runtime/controltarget
chmod 0555 "$binary"

# Mark attempts before calling Docker: a lost response may still have created
# the resource. Cleanup checks exact ownership labels before removing it.
volume_created=true
docker volume create \
  --label "sandbox.test.project=$project" \
  --label "sandbox.test.fixture=$fixture" \
  "$volume" >/dev/null
container_created=true
docker container create \
  --name "$container" \
  --label "sandbox.test.project=$project" \
  --label "sandbox.test.fixture=$fixture" \
  --network none --user 0:0 --read-only \
  --cap-drop ALL --cap-add CHOWN --security-opt no-new-privileges \
  --mount "type=bind,source=$binary,target=/journal-test,readonly" \
  --mount "type=volume,source=$volume,target=/tmp" \
  --entrypoint /journal-test \
  "$image" -test.v -test.count=1 >/dev/null

set +e
docker container start --attach "$container" 2>&1 | tee "$output"
start_result=${PIPESTATUS[0]}
test_state="$(docker container inspect --format '{{.State.Status}} {{.State.Running}} {{.State.ExitCode}}' "$container")"
inspect_result=$?
set -e
if [[ "$inspect_result" != 0 ]]; then
  echo "Unable to observe actual test exit; attach result=$start_result" >&2
  exit "$inspect_result"
fi
# ExitCode can be a default zero before termination; accept only a complete
# terminal-state inspection with a valid process exit code.
if [[ ! "$test_state" =~ ^exited\ false\ (0|[1-9][0-9]{0,2})$ ]]; then
  echo "Unable to observe completed test; state=$test_state attach result=$start_result" >&2
  exit 2
fi
actual_result=${BASH_REMATCH[1]}
if (( actual_result > 255 )); then
  echo "Invalid actual test exit: $actual_result; attach result=$start_result" >&2
  exit 2
fi
echo "Linux native test state=exited running=false actual_exit=$actual_result attach_exit=$start_result"
awk '/^--- FAIL:/ { failures++ } /^[[:space:]]*--- SKIP:/ { skips++ } END { printf "Linux native top-level failures=%d skips=%d (host race is separate)\n", failures, skips }' "$output"
if [[ "$actual_result" == 0 && "$start_result" != 0 ]]; then
  exit "$start_result"
fi
exit "$actual_result"
