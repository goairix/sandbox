#!/usr/bin/env bash
# New archive selectors only; controller-frozen Linux binaries are required.
set -euo pipefail
[[ $# == 5 && -f "$3" && -f "$4" && -f "$5" ]] || { echo 'usage: test-runtime-supervisor-archive.sh EVIDENCE tls|transfer|blocked|close|delivery TEST_ELF MONITOR_ELF USER_ELF' >&2; exit 2; }
image='gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
project="sandbox-supervisor-archive-$$-$(date +%s)"
fixture=supervisor-archive
mkdir -p "$1"
evidence=$(cd "$1" && pwd)
case_name="$2"
expected=0
case "$case_name" in
  tls) selector='^TestTaskArchiveNativeTLS$';;
  transfer) selector='^TestTaskArchiveNativeAdmissionTransfer$';;
  blocked) selector='^TestTaskArchiveNativeBlockedDeadline$'; expected=70;;
  close) selector='^TestTaskArchiveNativePhysicalClose$';;
  delivery) selector='^TestTaskArchiveNativeCompletionDeliveryFailure$';;
  *) exit 2;;
esac
containers=()
volumes=()
cleanup() {
  local original=$? failed=0 name labels remaining kind
  trap - EXIT INT TERM
  for name in ${containers[@]+"${containers[@]}"}; do
    labels=$(docker inspect --format '{{index .Config.Labels "sandbox.test.project"}} {{index .Config.Labels "sandbox.test.fixture"}}' "$name") || { failed=1; continue; }
    [[ "$labels" == "$project $fixture" ]] || { failed=1; continue; }
    docker rm -f "$name" >>"$evidence/cleanup.log" 2>&1 || failed=1
  done
  for name in ${volumes[@]+"${volumes[@]}"}; do
    labels=$(docker volume inspect --format '{{index .Labels "sandbox.test.project"}} {{index .Labels "sandbox.test.fixture"}}' "$name") || { failed=1; continue; }
    [[ "$labels" == "$project $fixture" ]] || { failed=1; continue; }
    docker volume rm "$name" >>"$evidence/cleanup.log" 2>&1 || failed=1
  done
  for kind in container volume network; do
    case "$kind" in
      container) remaining=$(docker ps -aq --filter "label=sandbox.test.project=$project") || failed=1;;
      volume) remaining=$(docker volume ls -q --filter "label=sandbox.test.project=$project") || failed=1;;
      network) remaining=$(docker network ls -q --filter "label=sandbox.test.project=$project") || failed=1;;
    esac
    printf '%s owned inventory: %s\n' "$kind" "$remaining" >>"$evidence/cleanup.log"
    [[ -z "$remaining" ]] || failed=1
  done
  (( original == 0 && failed == 0 )) || exit 1
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker image inspect --format '{"id":{{json .Id}},"digests":{{json .RepoDigests}},"os":{{json .Os}},"architecture":{{json .Architecture}}}' "$image" >"$evidence/image.json"
volume="$project-payload"
docker volume create --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" "$volume" >"$evidence/volume.txt"
volumes+=("$volume")
loader="$project-loader"
docker create --pull=never --name "$loader" --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" --network none --read-only --cap-drop ALL --security-opt no-new-privileges:true --mount "type=volume,src=$volume,dst=/payload" --entrypoint /payload/native.test "$image" >"$evidence/loader.txt"
containers+=("$loader")
name="$project-$case_name"
docker create --pull=never --name "$name" --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" \
  --network none --user 0:0 --read-only --memory 128m --pids-limit 128 --cpus 0.5 \
  --cap-drop ALL --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges:true \
  --mount "type=volume,src=$volume,dst=/payload,readonly" \
  --tmpfs /journal:rw,nosuid,nodev,noexec,size=32m,mode=0700 \
  --tmpfs /workspace:rw,nosuid,nodev,noexec,size=8m,mode=0777 \
  --env SANDBOX_TASK3_NATIVE=1 --env SANDBOX_TASK3_ATTESTATION=/payload/attestation.json \
  --env SANDBOX_TASK3_MONITOR=/payload/monitor --env TMPDIR=/journal --env GOMAXPROCS=2 \
  --entrypoint /payload/native.test "$image" -test.v -test.count=1 -test.timeout=45s -test.run "$selector" >"$evidence/container.txt"
containers+=("$name")
docker inspect --format '{"id":{{json .Id}},"image":{{json .Image}},"user":{{json .Config.User}},"network":{{json .HostConfig.NetworkMode}},"readonly":{{json .HostConfig.ReadonlyRootfs}},"cap_drop":{{json .HostConfig.CapDrop}},"cap_add":{{json .HostConfig.CapAdd}},"security":{{json .HostConfig.SecurityOpt}},"pid_mode":{{json .HostConfig.PidMode}},"userns_mode":{{json .HostConfig.UsernsMode}},"privileged":{{json .HostConfig.Privileged}},"memory":{{json .HostConfig.Memory}},"pids_limit":{{json .HostConfig.PidsLimit}},"mounts":{{json .Mounts}},"tmpfs":{{json .HostConfig.Tmpfs}}}' "$name" >"$evidence/created.json"
python3 - "$evidence" "$3" "$4" "$5" <<'PY'
import hashlib,json,sys,tarfile,io,os
directory=sys.argv[1]
observed=json.load(open(directory+'/created.json'))
image=json.load(open(directory+'/image.json'))
assert image['os']=='linux' and image['architecture']=='arm64' and 'gcr.io/etcd-development/etcd@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1' in image['digests']
assert observed['image']==image['id'] and observed['network']=='none' and observed['readonly'] and not observed['privileged'] and observed['user']=='0:0'
assert observed['cap_drop']==['ALL'] and set(observed['cap_add'])=={'CAP_KILL','CAP_SETUID','CAP_SETGID','CAP_SETPCAP'} and observed['security']==['no-new-privileges:true']
assert observed['pid_mode']=='' and observed['userns_mode']=='' and observed['memory']==134217728 and observed['pids_limit']==128
mounts=[{'type':m['Type'],'destination':m['Destination'],'rw':m['RW']} for m in observed['mounts']]
assert mounts==[{'type':'volume','destination':'/payload','rw':False}]
assert set(observed['tmpfs'])=={'/journal','/workspace'}
contract={k:v for k,v in observed.items() if k not in ('id','image','mounts')}
contract['mounts']=mounts
contract_json=json.dumps(contract,sort_keys=True,separators=(',',':'))
attestation={'version':1,'runtime_id':observed['id'],'runtime_uid':observed['id'],'image_id':observed['image'],'architecture':image['architecture'],'contract_digest':hashlib.sha256(contract_json.encode()).hexdigest(),'contract_json':contract_json}
data=json.dumps(attestation,sort_keys=True,separators=(',',':')).encode()
open(directory+'/attestation.json','wb').write(data)
with tarfile.open(directory+'/payload.tar','w') as archive:
    for name,path in zip(('native.test','monitor','archive-user'),sys.argv[2:]):
        entry=tarfile.TarInfo(name);entry.size=os.path.getsize(path);entry.mode=0o755;entry.uid=entry.gid=0
        with open(path,'rb') as source:archive.addfile(entry,source)
    entry=tarfile.TarInfo('attestation.json');entry.size=len(data);entry.mode=0o600;entry.uid=entry.gid=0;archive.addfile(entry,io.BytesIO(data))
PY
docker cp - "$loader:/payload" <"$evidence/payload.tar"
shasum -a 256 "$3" "$4" "$5" "$evidence/payload.tar" "$evidence/attestation.json" >"$evidence/payload-sha256.txt"
docker start -a "$name" >"$evidence/native.log" 2>&1 &
attach=$!
limit=$((SECONDS+60))
while kill -0 "$attach" 2>/dev/null; do
  if (( SECONDS >= limit )); then
    docker kill "$name" >>"$evidence/native.log" 2>&1 || true
    kill -TERM "$attach" 2>/dev/null || true
    grace=$((SECONDS+2))
    while kill -0 "$attach" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
    forced=false
    if kill -0 "$attach" 2>/dev/null; then
      forced=true
      kill -KILL "$attach" 2>/dev/null || true
      grace=$((SECONDS+2))
      while kill -0 "$attach" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
    fi
    if kill -0 "$attach" 2>/dev/null; then
      printf 'attachPID=%s forced=%s joined=false\n' "$attach" "$forced" >"$evidence/termination.txt"
      exit 1
    fi
    wait "$attach" || true
    printf 'attachPID=%s forced=%s joined=true\n' "$attach" "$forced" >"$evidence/termination.txt"
    exit 1
  fi
  sleep 0.2
done
attach_exit=0
wait "$attach" || attach_exit=$?
docker inspect --format '{"id":{{json .Id}},"state":{{json .State}}}' "$name" >"$evidence/exited.json"
terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}} {{.State.Restarting}} {{.State.OOMKilled}} {{.State.ExitCode}}' "$name")
printf 'attachExit=%s terminal=%s\n' "$attach_exit" "$terminal" >"$evidence/result.txt"
[[ "$terminal" == 'exited false false '* ]] || { echo 'processWait=not-terminal' >>"$evidence/result.txt"; exit 1; }
waited=$(docker wait "$name")
printf 'waitCommandExit=0 processWait=%s\n' "$waited" >>"$evidence/result.txt"
[[ "$attach_exit" == "$expected" && "$terminal" == "exited false false false $expected" && "$waited" == "$expected" ]] || exit 1
if [[ "$expected" == 70 ]]; then
  ! grep -Fxq PASS "$evidence/native.log"
  grep -Fxq 'TASK_ARCHIVE_BLOCKED_ARMED PID=1 actual_TLS_nonreading_DATA=true expected_exit=70' "$evidence/native.log"
  echo 'EXPECTED_ISOLATION_EXIT70; no Go PASS claim' >"$evidence/counts.txt"
  if grep -q 'sandbox-isolation ' "$evidence/native.log"; then
    echo 'bestEffortDiagnostic=present' >>"$evidence/counts.txt"
  else
    echo 'bestEffortDiagnostic=absent' >>"$evidence/counts.txt"
  fi
else
  grep -Fxq PASS "$evidence/native.log"
  counts=$(awk '/^[[:space:]]*--- PASS:/ {p++} /^[[:space:]]*--- FAIL:/ {f++} /^[[:space:]]*--- SKIP:/ {s++} END {printf "PASS=%d FAIL=%d SKIP=%d",p,f,s}' "$evidence/native.log")
  echo "$counts" >"$evidence/counts.txt"
  [[ "$counts" == 'PASS=1 FAIL=0 SKIP=0' ]] || exit 1
  if [[ "$case_name" == tls || "$case_name" == transfer ]]; then
    awk '/archive TAR base64:/ {print $NF}' "$evidence/native.log" | python3 -c 'import base64,sys;sys.stdout.buffer.write(base64.b64decode(sys.stdin.read().strip(),validate=True))' >"$evidence/archive.tar"
    awk '/archive receipt base64:/ {print $NF}' "$evidence/native.log" | python3 -c 'import base64,sys;sys.stdout.buffer.write(base64.b64decode(sys.stdin.read().strip(),validate=True))' >"$evidence/receipt.json"
    shasum -a 256 "$evidence/archive.tar" "$evidence/receipt.json" >"$evidence/received-sha256.txt"
  fi
fi
echo "selected archive fixture complete expectedExit=$expected evidence=$evidence"
