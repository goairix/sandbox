// No network, Secret reads, or Kubernetes writes: driver safety gates only.
const assert = require('node:assert/strict');
const {assertNamespaceIdentity, assertPodIdentity, validateOptions, buildLabValues, selectAuditEvidence} = require('./all-node-api');

const expected = {name: 'sandbox-aa-api-a18c24ff-2', uid: 'namespace-uid', run: 'a18c24ff', node: 'ds-ai-worker-2'};
const namespace = {metadata: {name: expected.name, uid: expected.uid, labels: {
  'sandbox-test-run': expected.run, 'sandbox-test-purpose': 'apparmor-all-node-api',
}}};
assert.doesNotThrow(() => assertNamespaceIdentity(namespace, expected));
for (const change of [
  {uid: 'replacement'}, {name: 'aiadp-sandbox-fuse'}, {labels: {'sandbox-test-run': 'wrong'}},
  {labels: {...namespace.metadata.labels, 'sandbox-test-purpose': 'wrong'}},
]) assert.throws(() => assertNamespaceIdentity({metadata: {...namespace.metadata, ...change}}, expected), /identity mismatch/);
const pod = {metadata: {uid: 'pod-uid', namespace: expected.name}, spec: {nodeName: expected.node}, status: {
  containerStatuses: [{restartCount: 0}], initContainerStatuses: [{restartCount: 0}],
}};
assert.doesNotThrow(() => assertPodIdentity(pod, expected, 'pod-uid'));
assert.throws(() => assertPodIdentity({...pod, metadata: {...pod.metadata, uid: 'replacement'}}, expected, 'pod-uid'), /identity mismatch/);
assert.throws(() => assertPodIdentity({...pod, spec: {nodeName: 'ds-ai-worker-3'}}, expected, 'pod-uid'), /identity mismatch/);
assert.throws(() => assertPodIdentity({...pod, status: {containerStatuses: [{restartCount: 1}]}}, expected, 'pod-uid'), /identity mismatch/);
assert.throws(() => assertPodIdentity({...pod, metadata: {...pod.metadata, deletionTimestamp: '2026-09-16T00:00:00Z'}}, expected, 'pod-uid'), /identity mismatch/);
assert.throws(() => validateOptions({execute: true, context: 'other', node: expected.node}), /explicit/);
assert.throws(() => validateOptions({execute: true, context: 'ds-ai-research', node: 'ds-ai-worker-6'}), /explicit/);
assert.doesNotThrow(() => validateOptions({execute: true, context: 'ds-ai-research', node: expected.node}));
assert.throws(() => buildLabValues(expected, {useSSL: false}), /verified TLS/);
const values = buildLabValues(expected, {useSSL: true, bucket: 'lab', endpoint: 'trusted.example', accessKey: 'private-a', secretKey: 'private-b'});
assert.equal(values.replicaCount, 1);
assert.equal(values.redis.mode, 'standalone');
assert.equal(values.redis.persistence.enabled, false);
assert.equal(values.config.workspace.allowMissingLSMForKind, false);
assert.equal(values.config.runtime.kubernetes.nodeSelector['kubernetes.io/hostname'], expected.node);
assert.equal(values.config.storage.filesystem.subPath, `validation/apparmor-all-nodes/${expected.run}/${expected.node}`);
const audit = [
  '[100.0] audit: apparmor="DENIED" operation="mknod" profile="exact" name="/dev/shm/marker-shadow" pid=10',
  '[100.1] audit: apparmor="DENIED" operation="open" profile="exact" name="/etc/shadow" pid=10',
  '[100.2] audit: apparmor="DENIED" operation="mknod" profile="exact" name="/dev/shm/marker-exec" pid=20',
  '[100.3] audit: apparmor="DENIED" operation="exec" profile="exact" name="/usr/bin/dash" pid=20',
  '[100.4] audit: apparmor="DENIED" operation="mount" profile="exact" name="/var/cache/s3fs/marker-mount/" fstype="tmpfs" pid=30',
  '[1.0] audit: apparmor="DENIED" operation="exec" profile="exact" name="/usr/bin/dash" pid=20',
  '[100.5] audit: apparmor="DENIED" operation="open" profile="business" name="/etc/shadow" pid=10',
].join('\n');
assert.deepEqual(selectAuditEvidence(audit, 'exact', 'marker', 99), {shadow: true, write: true, exec: true, mount: true});
assert.equal(selectAuditEvidence(audit, 'exact', 'missing', 99).shadow, false);
assert.equal(selectAuditEvidence(audit, 'exact', 'marker', 101).exec, false);
assert.equal(selectAuditEvidence(audit.replace('operation="mknod"', 'operation="open" requested_mask="r"'), 'exact', 'marker', 99).write, false);
assert.equal(selectAuditEvidence(audit.replace('operation="mknod"', 'operation="open" requested_mask="w"'), 'exact', 'marker', 99).write, true);
assert.equal(selectAuditEvidence(audit.replace('pid=10\n', 'pid=11\n'), 'exact', 'marker', 99).shadow, false);
assert.equal(require('./all-node-api').kernelAuditWatermark('[ 80.1] kernel: initial\n[ 82.4] audit: other-profile\n'), 82.4);
assert.throws(() => require('./all-node-api').kernelAuditWatermark('missing timestamps'), /watermark unavailable/);
assert.deepEqual(selectAuditEvidence(audit, 'different-profile', 'marker', 99), {shadow: false, write: false, exec: false, mount: false});
console.log('all-node driver identity, TLS isolation and audit correlation: PASS');

// Exercise the actual mutation entry points with a stubbed process transport.
const fs = require('node:fs');
const vm = require('node:vm');
const file = require('node:path').join(__dirname, 'all-node-api.js');
let currentNamespace = namespace, currentPod = pod, writes = 0;
const sandbox = {module: {exports: {}}, __dirname, console, Buffer, URL, setTimeout,
  require(name) {
    if (name !== 'node:child_process') return require(name);
    return {spawnSync(binary, args) {
      assert.equal(binary, 'kubectl');
      if (args.includes('get')) return {status: 0, stdout: JSON.stringify(args.includes('namespace') ? currentNamespace : currentPod)};
      writes++;
      return {status: 0, stdout: '', stderr: ''};
    }};
  }};
vm.runInNewContext(fs.readFileSync(file, 'utf8'), sandbox, {filename: file});
const Lab = sandbox.module.exports.Lab;
assert.equal(typeof Lab, 'function');
const lab = Object.create(Lab.prototype);
lab.expected = expected;
lab.release = 'aa-api-a18c24ff';
const uri = `/api/v1/namespaces/${expected.name}/pods/own-loader`;
const mutationCases = [
  () => lab.create([{kind: 'Secret', metadata: {name: 'own', namespace: expected.name}}]),
  () => lab.exec({...pod, metadata: {...pod.metadata, name: 'own-loader'}}, 'test', ['/bin/true']),
  () => lab.deleteExact(uri, 'pod-uid'),
];
for (const change of [{uid: 'replacement'}, {name: 'aiadp-sandbox-fuse'}, {labels: {'sandbox-test-run': 'wrong'}},
  {labels: {...namespace.metadata.labels, 'sandbox-test-purpose': 'wrong'}}]) {
  currentNamespace = {metadata: {...namespace.metadata, ...change}};
  for (const mutate of mutationCases) assert.throws(mutate, /identity mismatch/);
}
currentNamespace = namespace;
for (const change of [{metadata: {...pod.metadata, uid: 'replacement'}}, {spec: {nodeName: 'ds-ai-worker-3'}}]) {
  currentPod = {...pod, ...change};
  for (const mutate of mutationCases.slice(1)) assert.throws(mutate, /identity mismatch/);
}
currentPod = pod;
assert.throws(() => lab.deleteExact('/api/v1/namespaces/aiadp-sandbox-fuse', expected.uid), /escaped lab scope/);
assert.throws(() => lab.create([{kind: 'Node', metadata: {name: 'own', namespace: expected.name}}]), /escaped lab scope/);
assert.equal(writes, 0, 'identity/scope rejection must occur before any write or exec');
lab.stage = () => {};
currentPod = {...pod, status: {containerStatuses: [{name: 'sandbox', restartCount: 0, imageID: 'registry/runtime@sha256:abc'}],
  initContainerStatuses: [{name: 'workspace-mounter', restartCount: 0, imageID: 'registry/mounter@sha256:def'}]}};
assert.doesNotThrow(() => lab.verifyImages(currentPod, {sandbox: 'registry/runtime:v1@sha256:abc', 'workspace-mounter': 'registry/mounter:v1@sha256:def'}));
assert.throws(() => lab.verifyImages(currentPod, {'workspace-mounter': 'registry/mounter:v1@sha256:wrong'}), /digest mismatch/);
console.log('actual mutation entry points reject replaced/out-of-scope targets with zero side effects: PASS');
Promise.all([
  assert.rejects(lab.object({useSSL: true}, '../business.txt'), /escaped lab scope/),
  assert.rejects(lab.object({useSSL: true}, 'final.txt', 200, 'PUT'), /escaped lab scope/),
  assert.rejects(lab.object({useSSL: false}, 'final.txt'), /escaped lab scope/),
]).then(() => console.log('object cleanup rejects escaped paths, methods and insecure TLS before requests: PASS'))
  .catch(error => {console.error(error); process.exitCode = 1;});
