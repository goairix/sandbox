#!/usr/bin/env bash
# Root-custodied native gate preparation. This script never discovers or starts
# Docker resources: the controller independently creates/inspects the exact
# labeled metadata, initializer, parent and production PID1 fixtures, consumes
# this frozen bundle and retains/cleans only those exact objects.
set -euo pipefail
if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo 'usage: test-authenticated-target.sh freeze|verify OWNED_OUTPUT [amd64|arm64]' >&2
  exit 2
fi
python3 - "$@" <<'PY'
import hashlib,json,os,pathlib,shutil,subprocess,sys,time
mode,target,*archargs=sys.argv[1:]
root=pathlib.Path(subprocess.check_output(['git','rev-parse','--show-toplevel'],text=True).strip())
own=root/'.superpowers/sdd/2026-10-07-authenticated-target-execution'
out=pathlib.Path(target).resolve()
if out.parent!=own.resolve() or not out.name.startswith('task-4-freeze-'):
    raise SystemExit('output must be a new task-4-freeze-* directory directly inside the owned task scratch')
def digest(path): return hashlib.sha256(path.read_bytes()).hexdigest()
manifest_path=out/'manifest.json'
if mode=='verify':
    manifest=json.loads(manifest_path.read_text())
    for name,want in manifest['sources'].items():
        if digest(root/name)!=want or digest(out/'source'/name)!=want: raise SystemExit('source drift: '+name)
    for name,want in manifest['executables'].items():
        if digest(out/name)!=want: raise SystemExit('executable drift: '+name)
    for name,want in manifest['compiler'].items():
        if digest(pathlib.Path(name))!=want: raise SystemExit('compiler drift: '+name)
    print('FROZEN_SOURCE_AND_ELF_MATCH',manifest_path)
    raise SystemExit(0)
if mode!='freeze' or len(archargs)!=1 or archargs[0] not in ('amd64','arm64'): raise SystemExit('freeze requires exact supported architecture')
arch=archargs[0]
out.mkdir(mode=0o700)
paths=subprocess.check_output(['rg','--files','-g','*.go','-g','*.sh','-g','go.mod','-g','go.sum'],cwd=root,text=True).splitlines()
manifest={'version':1,'started_utc':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),'base':'a671f93d2b00f2b31c32889a8d5b8c9f1bc129c1','head':subprocess.check_output(['git','rev-parse','HEAD'],cwd=root,text=True).strip(),'sources':{},'executables':{},'compiler':{},'selectors':['^TestAuthenticatedTargetInitialize$','^TestAuthenticatedTargetNativeSuccess$','^TestAuthenticatedTargetNativeBridge$','^TestAuthenticatedTargetNativeFences$','^TestAuthenticatedTargetNativeIndexValue$','^TestAuthenticatedTargetNativeIndexLease$','^TestAuthenticatedTargetNativeIndexRecreation$','^TestAuthenticatedTargetNativeIssuerValue$','^TestAuthenticatedTargetNativeIssuerLease$','^TestAuthenticatedTargetNativeIssuerRecreation$','^TestAuthenticatedTargetNativeLifecycle$','^TestAuthenticatedTargetNativeFrames$','^TestAuthenticatedTargetNativeStreamLoss$','^TestAuthenticatedTargetNativeStartFailure$'],'scope':'single-member native metadata and actual production PID1; no fleet or three-member failover claim'}
for name in sorted(paths):
    source=root/name
    manifest['sources'][name]=digest(source)
    copy=out/'source'/name;copy.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(source,copy)
preflight_cmd=['go','env','-json','GOVERSION','GOROOT','GOTOOLDIR','GOTOOLCHAIN','GOFLAGS']
preflight=subprocess.run(preflight_cmd,cwd=root,capture_output=True,text=True)
(out/'toolchain-preflight.json').write_text(json.dumps({'argv':preflight_cmd,'exit':preflight.returncode,'stdout':preflight.stdout,'stderr':preflight.stderr},indent=2))
if preflight.returncode: raise SystemExit('existing toolchain resolution failed')
resolved=json.loads(preflight.stdout)
selected_go=pathlib.Path(resolved['GOROOT'])/'bin/go'
env=os.environ.copy();env.update(GOOS='linux',GOARCH=arch,CGO_ENABLED='0',GOTOOLCHAIN='local',GOMAXPROCS='2',GOROOT=resolved['GOROOT'])
manifest['go_env']=json.loads(subprocess.check_output([str(selected_go),'env','-json','GOOS','GOARCH','CGO_ENABLED','GOVERSION','GOROOT','GOTOOLDIR','GOMOD','GOFLAGS'],cwd=root,env=env,text=True))
for tool in [selected_go,pathlib.Path(manifest['go_env']['GOTOOLDIR'])/'compile',pathlib.Path(manifest['go_env']['GOTOOLDIR'])/'link']:
    manifest['compiler'][str(tool.resolve())]=digest(tool)
commands=[('sandbox-launcher',[str(selected_go),'build','-mod=readonly','-o',str(out/'sandbox-launcher'),'./cmd/sandbox-launcher']),('user',[str(selected_go),'build','-mod=readonly','-o',str(out/'user'),'./internal/runtime/controlrunner/testdata/user']),('etcd.test',[str(selected_go),'test','-mod=readonly','-c','-o',str(out/'etcd.test'),'./internal/storage/state/etcd'])]
for name,cmd in commands:
    start=time.monotonic();utc=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime());result=subprocess.run(cmd,cwd=root,env=env,capture_output=True,text=True)
    (out/(name+'.build.json')).write_text(json.dumps({'argv':cmd,'environment':{k:env[k] for k in ('GOOS','GOARCH','CGO_ENABLED','GOTOOLCHAIN','GOMAXPROCS','GOROOT')},'GOFLAGS':manifest['go_env']['GOFLAGS'],'start_utc':utc,'duration_seconds':time.monotonic()-start,'exit':result.returncode,'stdout':result.stdout,'stderr':result.stderr},indent=2))
    if result.returncode: raise SystemExit(result.stdout+result.stderr)
    manifest['executables'][name]=digest(out/name)
for name,want in manifest['sources'].items():
    if digest(root/name)!=want: raise SystemExit('source changed during freeze: '+name)
manifest['finished_utc']=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime());manifest_path.write_text(json.dumps(manifest,indent=2));print('FROZEN_NATIVE_CANDIDATE',manifest_path)
PY
