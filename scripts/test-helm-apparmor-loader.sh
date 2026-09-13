#!/usr/bin/env bash
set -euo pipefail
repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
python3 - "$repo_root/deploy/helm/sandbox" <<'PY'
import hashlib
import json
import pathlib
import subprocess
import sys
import yaml

chart = sys.argv[1]
def render(overrides):
    command = ['helm', 'template', 'sandbox', chart, '--namespace', 'release-ns']
    for key, value in overrides.items():
        command += ['--set-json', key + '=' + json.dumps(value)]
    return [doc for doc in yaml.safe_load_all(subprocess.check_output(command, text=True)) if doc]

def env(container):
    entries = container.get('env', [])
    names = [entry['name'] for entry in entries]
    assert len(names) == len(set(names)), 'duplicate env'
    return {entry['name']: entry.get('value') for entry in entries}

default = render({})
assert not any(doc['kind'] == 'DaemonSet' for doc in default), 'default loader must stay disabled'
priority_docs = render({'apparmorLoader.enabled': True, 'apparmorLoader.priorityClassName': 'sandbox-node-loader'})
priority_ds = next(doc for doc in priority_docs if doc['kind'] == 'DaemonSet')
assert priority_ds['spec']['template']['spec'].get('priorityClassName') == 'sandbox-node-loader', 'explicit loader PriorityClass missing'
def fingerprint(docs):
    deployment = next(doc for doc in docs if doc['kind'] == 'Deployment')
    return deployment['spec']['template']['metadata']['annotations']['goairix.github.io/sandbox-backend-fingerprint']

assert fingerprint(default) != fingerprint(render({'config.workspace.lsmProfile': 'another-manual-profile'})), 'manual LSM must change backend contract'
assert fingerprint(default) != fingerprint(render({'config.runtime.kubernetes.nodeSelector': {'zone': 'one'}})), 'selector must change backend contract'
assert fingerprint(render({'config.runtime.kubernetes.nodeSelector': {'zone': 'one', 'pool': 'gpu'}})) == fingerprint(render({'config.runtime.kubernetes.nodeSelector': {'pool': 'gpu', 'zone': 'one'}})), 'selector encoding must be stable'
production = render({'apparmorLoader.enabled': True, 'config.workspace.lsmProfile': '',
                     'productionSafetyChecks': True, 'redis.enabled': False,
                     'redis.external.mode': 'sentinel', 'redis.external.masterName': 'sandbox',
                     'redis.external.addrs': ['observer-a:26379', 'observer-b:26379', 'observer-c:26379'],
                     'redis.external.requireHA': True, 'redis.external.durability': 'replica_ack'})
assert any(doc['kind'] == 'DaemonSet' for doc in production), 'production validation must use effective automatic profile'
for namespace in ['', 'runtime-ns']:
    docs = render({'apparmorLoader.enabled': True,
                   'config.workspace.lsmProfile': '',
                   'config.runtime.kubernetes.namespace': namespace,
                   'config.runtime.kubernetes.nodeSelector': {'node.example/pool': 'sandbox'}})
    ds = next((doc for doc in docs if doc['kind'] == 'DaemonSet'), None)
    assert ds is not None, 'enabled loader DaemonSet missing'
    cm = next(doc for doc in docs if doc['kind'] == 'ConfigMap' and 'profile' in doc.get('data', {}))
    template = pathlib.Path(chart, 'files/apparmor/workspace-mounter.profile').read_text()
    canonical = template.replace('\r\n', '\n').strip() + '\n'
    digest = hashlib.sha256(canonical.encode()).hexdigest()
    profile = 'sandbox-fuse-' + digest
    assert canonical.count('__SANDBOX_PROFILE_NAME__') == 1
    assert cm['data']['profile'] == canonical.replace('__SANDBOX_PROFILE_NAME__', profile)
    pod = ds['spec']['template']
    assert all(len(value) <= 63 for value in pod['metadata']['labels'].values()), 'invalid Kubernetes label length'
    assert ds['metadata']['annotations']['sandbox.apparmor.profile-digest'] == digest
    assert pod['metadata']['labels']['sandbox.apparmor.profile-digest'] == digest[:63]
    assert pod['metadata']['annotations']['sandbox.apparmor.profile-digest'] == digest
    spec = pod['spec']
    selector = {'node.example/pool': 'sandbox', 'kubernetes.io/os': 'linux'}
    assert spec['nodeSelector'] == selector
    assert spec['automountServiceAccountToken'] is False
    assert not any(spec.get(key, False) for key in ['hostPID', 'hostIPC', 'hostNetwork'])
    container = spec['containers'][0]
    assert container['name'] == 'apparmor-loader'
    assert container['securityContext']['privileged'] is True
    assert container['securityContext']['readOnlyRootFilesystem'] is True
    assert '--profile-name=' + profile in container['args']
    assert '--profile-digest=' + digest in container['args']
    mounts = {entry['name']: entry for entry in container['volumeMounts']}
    host_paths = {volume['name']: volume['hostPath']['path'] for volume in spec['volumes'] if 'hostPath' in volume}
    assert set(host_paths.values()) == {'/sys/kernel/security', '/sys/module/apparmor/parameters/enabled'}
    assert mounts['module-enabled']['readOnly'] is True
    assert mounts['profile']['readOnly'] is True
    assert container['readinessProbe']['exec']['command'][1] == 'readiness'
    deployment = next(doc for doc in docs if doc['kind'] == 'Deployment')
    api_env = env(deployment['spec']['template']['spec']['containers'][0])
    assert api_env['SANDBOX_WORKSPACE_BACKEND_LSM_PROFILE'] == profile
    assert json.loads(api_env['SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR']) == selector
    assert api_env['SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_NAME'] == ds['metadata']['name']
    assert api_env['SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_NAMESPACE'] == 'release-ns'
    for job in (doc for doc in docs if doc['kind'] == 'Job'):
        for c in job['spec']['template']['spec']['containers']:
            if c['name'] == 'drain':
                values = env(c)
                assert values['SANDBOX_WORKSPACE_BACKEND_LSM_PROFILE'] == profile
                assert json.loads(values['SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR']) == selector
    role = next(doc for doc in docs if doc['kind'] == 'Role' and doc['metadata']['name'] == 'sandbox-apparmor-reader')
    ds_rule = next(rule for rule in role['rules'] if 'daemonsets' in rule['resources'])
    assert ds_rule == {'apiGroups': ['apps'], 'resources': ['daemonsets'], 'verbs': ['get'], 'resourceNames': [ds['metadata']['name']]}
    pod_rules = [rule for rule in role['rules'] if 'pods' in rule['resources']]
    assert bool(pod_rules) == bool(namespace)
    if pod_rules:
        assert pod_rules == [{'apiGroups': [''], 'resources': ['pods'], 'verbs': ['get', 'list']}]
    sa = next(doc for doc in docs if doc['kind'] == 'ServiceAccount' and doc['metadata']['name'] == spec['serviceAccountName'])
    assert sa['automountServiceAccountToken'] is False

for overrides in [
    {'config.workspace.enabledMountModes': ['sync']},
    {'config.workspace.allowMissingLSMForKind': True},
    {'config.runtime.kubernetes.nodeSelector': {'kubernetes.io/os': 'windows'}},
    {'config.runtime.kubernetes.nodeSelector': {'node.example/pool': True}},
    {'apparmorLoader.checkIntervalSeconds': 0},
    {'apparmorLoader.checkIntervalSeconds': True},
    {'apparmorLoader.enabled': 'false'},
    {'apparmorLoader.priorityClassName': True},
    {'apparmorLoader.priorityClassName': 'bad..class'},
    {'apparmorLoader.priorityClassName': 'a' * 64},
    {'config.runtime.kubernetes.nodeSelector': {'example.com/k' + str(i): 'v' for i in range(64)}},
]:
    command = ['helm', 'template', 'sandbox', chart]
    for key, value in {'apparmorLoader.enabled': True, **overrides}.items(): command += ['--set-json', key + '=' + json.dumps(value)]
    result = subprocess.run(command, capture_output=True, text=True)
    assert result.returncode != 0, ('unsafe loader configuration accepted', overrides)
print('helm AppArmor loader tests: PASS')
PY
