// Explicitly opted-in, bounded, per-node lab. Never print Secret/env/error bodies.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const http = require('node:http');
const https = require('node:https');
const {spawn, spawnSync} = require('node:child_process');

const CONTEXT = 'ds-ai-research';
const BUSINESS_NS = 'aiadp-sandbox-fuse';
const RELEASE = 'sandbox-fuse';
const PURPOSE = 'apparmor-all-node-api';
const DIGEST = '37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d';
const PROFILE = `sandbox-fuse-${DIGEST}`;
const REPO = 'registry.i.huaxisy.com/library/ai-infra';
const IMAGES = {
  api: `${REPO}/sandbox-api:v0.3.31@sha256:1e8e95347f805417998d4555d6d107d37e0895929bed8d095b33eda79f15cd4a`,
  runtime: `${REPO}/sandbox-runtime:v0.3.4@sha256:237bd749531b31eb6144b5ce2d4b6dc93232c39bae2989c6bb9a4797a575e8c8`,
  mounter: `${REPO}/sandbox-fuse-mounter:v0.3.30@sha256:56db3a0e82c36259ff5325cf5294a5c3b876c085b8ccde147c41c76bca2e8931`,
  loader: `${REPO}/sandbox-apparmor-loader:v0.3.24@sha256:5a984dcb978f6634acee4446bb2f1ae5d2aca918ee53b189372deb688bcf908c`,
  redis: 'registry.i.huaxisy.com/library/redis:7.4.2@sha256:144902e12778c0b4ad2b1812948a05c56ffd3977299fbd201aa628844ff17495',
};
const CHART = path.resolve(__dirname, '../../deploy/helm/sandbox');
const hash = value => crypto.createHash('sha256').update(value).digest('hex');
const sleep = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));
function check(condition, message) { if (!condition) throw new Error(message); }

function validateOptions(options) {
  check(options.execute && options.context === CONTEXT && /^ds-ai-worker-[1-5]$/.test(options.node || ''),
    'requires explicit --execute --context ds-ai-research --node ds-ai-worker-[1-5]');
}
function assertNamespaceIdentity(object, expected) {
  check(/^sandbox-aa-api-[a-f0-9]{8}-[1-5]$/.test(expected.name) &&
    object?.metadata?.name === expected.name && object.metadata.uid === expected.uid &&
    object.metadata.labels?.['sandbox-test-run'] === expected.run &&
    object.metadata.labels?.['sandbox-test-purpose'] === PURPOSE, 'lab namespace identity mismatch');
}
function assertPodIdentity(object, expected, uid) {
  const statuses = [...object?.status?.containerStatuses || [], ...object?.status?.initContainerStatuses || []];
  check(uid && object?.metadata?.uid === uid && object.metadata.namespace === expected.name &&
    !object.metadata.deletionTimestamp && object.spec?.nodeName === expected.node &&
    statuses.length > 0 && statuses.every(status => status.restartCount === 0), 'lab Pod identity mismatch');
}
function imageValues(image) {
  const split = image.indexOf(':v');
  check(split > 0, 'lab image is not release pinned');
  return {repository: image.slice(0, split), tag: image.slice(split + 1), pullPolicy: 'IfNotPresent'};
}
function buildLabValues(expected, storage) {
  check(storage.useSSL === true && storage.bucket && storage.endpoint && storage.accessKey && storage.secretKey,
    'lab requires verified TLS storage configuration');
  return {
    replicaCount: 1, productionSafetyChecks: false,
    autoscaling: {enabled: false}, podDisruptionBudget: {enabled: false},
    image: imageValues(IMAGES.api),
    apparmorLoader: {enabled: true, image: imageValues(IMAGES.loader)},
    preDeleteDrain: {enabled: true, timeoutSeconds: 240},
    redis: {enabled: true, mode: 'standalone', persistence: {enabled: false}},
    config: {
      runtime: {type: 'kubernetes', kubernetes: {namespace: expected.name,
        nodeSelector: {'kubernetes.io/hostname': expected.node, 'kubernetes.io/os': 'linux'}, networkPolicyProvider: 'auto'}},
      pool: {minSize: 1, maxSize: 2}, images: {sandbox: IMAGES.runtime},
      storage: {filesystem: {...storage, preset: 'minio',
        storageIdentity: `aa-api-${expected.run}`, credentialGeneration: `aa-api-${expected.run}`,
        subPath: `validation/apparmor-all-nodes/${expected.run}/${expected.node}`}},
      workspace: {defaultMountMode: 'sync', enabledMountModes: ['sync', 'fuse'],
        allowMissingLSMForKind: false, fuseImages: {mounter: IMAGES.mounter},
        fusePool: {minSize: 1, maxSize: 2}, cacheSize: '256Mi',
        mounterResources: {ephemeralStorageRequest: '256Mi', ephemeralStorageLimit: '1Gi'}},
      security: {apiKeySecretName: 'lab-api-key'},
      telemetry: {tracer: {otlpEnabled: false}, metrics: {otlpEnabled: false}, log: {otlpEnabled: false}},
    },
  };
}

function selectAuditEvidence(text, profile, marker, since) {
  const lines = text.split('\n').filter(line => {
    const time = line.match(/^\[\s*([\d.]+)\]/);
    return time && Number(time[1]) >= since && line.includes('apparmor="DENIED"') && line.includes(`profile="${profile}"`);
  });
  const pidFor = suffix => lines.filter(line => line.includes(`name="/dev/shm/${marker}-${suffix}"`))
    .map(line => line.match(/\bpid=(\d+)\b/)?.[1]).filter(Boolean);
  const correlated = (suffix, operation, name) => {
    const pids = pidFor(suffix);
    return lines.some(line => line.includes(`operation="${operation}"`) && line.includes(`name="${name}"`) &&
      pids.includes(line.match(/\bpid=(\d+)\b/)?.[1]));
  };
  return {
    shadow: correlated('shadow', 'open', '/etc/shadow'),
    write: lines.some(line => line.includes(`name="/dev/shm/${marker}-shadow"`) &&
      (line.includes('operation="mknod"') || line.includes('operation="open"') && /(?:requested|denied)_mask="[^"]*[cw]/.test(line))),
    exec: correlated('exec', 'exec', '/usr/bin/dash') || correlated('exec', 'exec', '/bin/dash') || correlated('exec', 'exec', '/bin/sh'),
    mount: lines.some(line => line.includes('operation="mount"') && line.includes('fstype="tmpfs"') &&
      line.includes(`/var/cache/s3fs/${marker}-mount`)),
  };
}
function kernelAuditWatermark(text) {
  const times = text.split('\n').map(line => Number(line.match(/^\[\s*([\d.]+)\]/)?.[1])).filter(Number.isFinite);
  check(times.length > 0, 'kernel audit monotonic watermark unavailable');
  return Math.max(...times);
}

function processCommand(binary, args, input, timeout = 30000) {
  const result = spawnSync(binary, args, {input, encoding: 'utf8', timeout, maxBuffer: 8 << 20});
  check(result.status === 0 && !result.error, 'lab command failed');
  return result.stdout;
}
function kube(args, input) { return processCommand('kubectl', ['--context', CONTEXT, '--request-timeout=25s', ...args], input); }
function get(kind, name, namespace) {
  const args = namespace ? ['-n', namespace] : [];
  return JSON.parse(kube([...args, 'get', kind, ...(name ? [name] : []), '-o', 'json']));
}
function ready(object) { return object?.status?.conditions?.some(condition => condition.type === 'Ready' && condition.status === 'True'); }
async function waitFor(read, predicate, label, budget = 240000) {
  const until = Date.now() + budget;
  do {
    const object = read();
    if (predicate(object)) return object;
    await sleep(3000);
  } while (Date.now() < until);
  throw new Error(`${label} timed out`);
}
function businessSnapshot() {
  return [['deployment', `${RELEASE}-api`], ['statefulset', `${RELEASE}-redis-sentinel`], ['daemonset', `${RELEASE}-apparmor-loader`]]
    .map(([kind, name]) => {
      const object = get(kind, name, BUSINESS_NS);
      const count = kind === 'daemonset' ? object.status.numberReady : object.status.readyReplicas;
      check(count === (kind === 'daemonset' ? 5 : 3), 'business workload is not healthy');
      const selector = Object.entries(object.spec.selector.matchLabels).map(([key, value]) => `${key}=${value}`).join(',');
      const pods = JSON.parse(kube(['-n', BUSINESS_NS, 'get', 'pods', '-l', selector, '-o', 'json'])).items;
      check(pods.length === count && pods.every(ready), 'business Pods are not stable and healthy');
      return {kind, name, uid: object.metadata.uid, templateHash: hash(JSON.stringify(object.spec.template)), count,
        pods: pods.map(pod => ({uid: pod.metadata.uid, images: (pod.status.containerStatuses || []).map(status =>
          ({name: status.name, imageID: status.imageID, restartCount: status.restartCount}))})).sort((a, b) => a.uid.localeCompare(b.uid))};
    });
}
function privateStorageConfiguration() {
  const deployment = get('deployment', `${RELEASE}-api`, BUSINESS_NS);
  const env = deployment.spec.template.spec.containers.find(container => container.name === 'api')?.env ||
    deployment.spec.template.spec.containers[0].env;
  function value(name) {
    const item = env.find(item => item.name === name);
    if (item?.value !== undefined) return item.value;
    const ref = item?.valueFrom?.secretKeyRef;
    if (ref) return Buffer.from(get('secret', ref.name, BUSINESS_NS).data[ref.key], 'base64').toString();
    return '';
  }
  check(value('SANDBOX_STORAGE_FILESYSTEM_PROVIDER') === 'minio', 'lab storage is not the validated MinIO backend');
  return {bucket: value('SANDBOX_STORAGE_FILESYSTEM_BUCKET'), region: value('SANDBOX_STORAGE_FILESYSTEM_REGION') || 'us-east-1',
    endpoint: value('SANDBOX_STORAGE_FILESYSTEM_ENDPOINT'), useSSL: value('SANDBOX_STORAGE_FILESYSTEM_USE_SSL') === 'true',
    accessKey: value('SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY'), secretKey: value('SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY')};
}

class Lab {
  constructor(node) {
    this.expected = {node, run: crypto.randomBytes(4).toString('hex')};
    this.expected.name = `sandbox-aa-api-${this.expected.run}-${node.slice(-1)}`;
    this.release = `aa-api-${this.expected.run}`;
    this.key = crypto.randomBytes(32).toString('hex');
    this.active = new Set();
    this.dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sandbox-aa-api-'));
    fs.chmodSync(this.dir, 0o700);
    this.report = {context: CONTEXT, node, run: this.expected.run, namespace: this.expected.name, images: IMAGES, stages: []};
  }
  stage(name, evidence = {}) {
    this.report.stages.push({name, at: new Date().toISOString(), ...evidence});
    fs.writeFileSync(path.join(this.dir, 'report.json'), JSON.stringify(this.report, null, 2), {mode: 0o600});
    console.log(`${this.expected.node}: ${name}`);
  }
  namespace() { assertNamespaceIdentity(get('namespace', this.expected.name), this.expected); }
  objects(kind, selector) {
    this.namespace();
    return JSON.parse(kube(['-n', this.expected.name, 'get', kind, ...(selector ? ['-l', selector] : []), '-o', 'json'])).items;
  }
  pod(name, uid) {
    this.namespace();
    const object = get('pod', name, this.expected.name);
    assertPodIdentity(object, this.expected, uid);
    return object;
  }
  exec(pod, container, argv, input, deny = false) {
    this.pod(pod.metadata.name, pod.metadata.uid);
    const result = spawnSync('kubectl', ['--context', CONTEXT, '--request-timeout=25s', '-n', this.expected.name,
      'exec', ...(input !== undefined ? ['-i'] : []), pod.metadata.name, '-c', container, '--', ...argv],
    {input, encoding: 'utf8', timeout: 30000, maxBuffer: 1 << 20});
    this.pod(pod.metadata.name, pod.metadata.uid);
    check(!result.error && result.status !== null, 'lab exec transport failed');
    if (deny) check(result.status !== 0 && /Permission denied|permission denied|Operation not permitted|write-protected|read-only/.test(result.stderr), 'expected AppArmor denial not observed');
    else check(result.status === 0, 'lab allowed operation failed');
    return result.stdout;
  }
  create(items) {
    this.namespace();
    for (const item of items) {
      const cluster = ['ClusterRole', 'ClusterRoleBinding'].includes(item.kind);
      const allowed = ['Secret', 'Service', 'ServiceAccount', 'ConfigMap', 'Role', 'RoleBinding', 'Deployment', 'StatefulSet', 'DaemonSet', 'NetworkPolicy', 'Job'];
      check(cluster ? item.metadata.name === `${this.expected.name}-${this.release}-network-inventory` :
        allowed.includes(item.kind) && item.metadata.namespace === this.expected.name, 'rendered resource escaped lab scope');
      item.metadata.labels = {...item.metadata.labels, 'sandbox-test-run': this.expected.run, 'sandbox-test-purpose': PURPOSE};
      if (cluster) item.metadata.ownerReferences = [{apiVersion: 'v1', kind: 'Namespace', name: this.expected.name, uid: this.expected.uid}];
      if (item.kind === 'ClusterRole') check(item.rules.every(rule => rule.verbs.every(verb => ['get', 'list'].includes(verb))), 'inventory RBAC is not read-only');
      if (item.kind === 'ClusterRoleBinding') check(item.roleRef.name === item.metadata.name &&
        item.subjects.every(subject => subject.kind === 'ServiceAccount' && subject.namespace === this.expected.name), 'inventory binding escaped lab scope');
    }
    kube(['create', '-f', '-'], JSON.stringify({apiVersion: 'v1', kind: 'List', items}));
  }
  render(templates) {
    const args = ['template', this.release, CHART, '--namespace', this.expected.name, '-f', path.join(this.dir, 'values.json')];
    for (const template of templates) args.push('--show-only', `templates/${template}.yaml`);
    const yaml = processCommand('helm', args);
    return JSON.parse(processCommand('ruby', ['-ryaml', '-rjson', '-e', 'puts JSON.generate(YAML.load_stream(STDIN.read).compact)'], yaml));
  }
  async setup(storage) {
    this.storage = storage;
    // Validate and render before the first Kubernetes mutation.
    const values = buildLabValues(this.expected, storage);
    values.redis.image = {repository: IMAGES.redis.slice(0, IMAGES.redis.indexOf(':7.')), tag: IMAGES.redis.slice(IMAGES.redis.indexOf(':7.') + 1), pullPolicy: 'IfNotPresent'};
    values.redis.password = crypto.randomBytes(32).toString('hex');
    fs.writeFileSync(path.join(this.dir, 'values.json'), JSON.stringify(values), {mode: 0o600});
    const items = this.render(['serviceaccount', 'rbac', 'runtime-role', 'runtime-rolebinding', 'runtime-network-inventory',
      'secret', 'service', 'redis', 'deployment', 'apparmor-loader', 'networkpolicy']);
    const namespace = JSON.parse(kube(['create', '-f', '-', '-o', 'json'], JSON.stringify({apiVersion: 'v1', kind: 'Namespace',
      metadata: {name: this.expected.name, labels: {'sandbox-test-run': this.expected.run, 'sandbox-test-purpose': PURPOSE}}})));
    this.expected.uid = namespace.metadata.uid;
    this.report.namespaceUID = this.expected.uid;
    this.stage('namespace-created');
    this.create([{apiVersion: 'v1', kind: 'Secret', metadata: {name: 'lab-api-key', namespace: this.expected.name},
      type: 'Opaque', stringData: {'api-key': this.key}}]);
    for (const item of items) {
      if (['Deployment', 'StatefulSet'].includes(item.kind)) item.spec.template.spec.nodeSelector = values.config.runtime.kubernetes.nodeSelector;
      if (item.kind === 'StatefulSet') {
        item.spec.template.spec.volumes = [{name: 'redis-data', emptyDir: {sizeLimit: '128Mi'}}];
        item.spec.template.spec.containers[0].volumeMounts = [{name: 'redis-data', mountPath: '/data'}];
      }
    }
    this.resourcesCreated = true;
    this.create(items);
    this.stage('chart-resources-created');
    await waitFor(() => this.objects('pods'), pods => pods.some(pod => pod.metadata.labels?.app === 'sandbox' &&
      pod.metadata.labels?.release === this.release && ready(pod)), 'API startup');
    this.apiPod = this.objects('pods', `app=sandbox,release=${this.release}`).find(ready);
    this.loader = this.objects('pods', `app=sandbox-apparmor-loader,release=${this.release}`).find(ready);
    check(this.loader, 'own loader is not Ready');
    this.pod(this.apiPod.metadata.name, this.apiPod.metadata.uid);
    this.pod(this.loader.metadata.name, this.loader.metadata.uid);
    this.verifyImages(this.apiPod, {sandbox: IMAGES.api});
    this.verifyImages(this.loader, {'apparmor-loader': IMAGES.loader});
    const redisPod = this.objects('pods', `app=sandbox-redis,release=${this.release}`).find(ready);
    check(redisPod, 'own Redis is not Ready');
    this.verifyImages(redisPod, {redis: IMAGES.redis});
    this.kernelProfile(this.loader);
    await this.startForward();
    await this.api('GET', '/ready');
    this.stage('API-loader-ready', {apiUID: this.apiPod.metadata.uid, loaderUID: this.loader.metadata.uid});
  }
  kernelProfile(loader) {
    const profiles = this.exec(loader, 'apparmor-loader', ['/bin/cat', '/sys/kernel/security/apparmor/profiles']);
    check(profiles.split('\n').includes(`${PROFILE} (enforce)`), 'exact kernel enforce profile missing');
  }
  verifyImages(pod, expected) {
    this.pod(pod.metadata.name, pod.metadata.uid);
    const identities = Object.entries(expected).map(([name, image]) => {
      const status = [...pod.status.containerStatuses || [], ...pod.status.initContainerStatuses || []].find(status => status.name === name);
      check(status?.imageID?.endsWith(image.slice(image.indexOf('@') + 1)), 'resolved image digest mismatch');
      return {name, imageID: status.imageID};
    });
    this.stage('resolved-images-verified', {podUID: pod.metadata.uid, identities});
  }
  async startForward() {
    this.pod(this.apiPod.metadata.name, this.apiPod.metadata.uid);
    this.forward = spawn('kubectl', ['--context', CONTEXT, '-n', this.expected.name, 'port-forward',
      '--address=127.0.0.1', `pod/${this.apiPod.metadata.name}`, ':8080'], {stdio: ['ignore', 'pipe', 'pipe']});
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('lab forward timed out')), 15000);
      this.forward.once('error', () => {clearTimeout(timer); reject(new Error('lab forward unavailable'));});
      this.forward.once('exit', () => {clearTimeout(timer); reject(new Error('lab forward ended'));});
      let output = '';
      this.forward.stdout.on('data', chunk => {
        output += chunk;
        const match = output.match(/Forwarding from 127\.0\.0\.1:(\d+)/);
        if (match) {this.port = Number(match[1]); clearTimeout(timer); resolve();}
      });
      this.forward.stderr.resume();
    });
  }
  async api(method, route, body) {
    this.pod(this.apiPod.metadata.name, this.apiPod.metadata.uid);
    const raw = body === undefined ? '' : JSON.stringify(body);
    const result = await new Promise((resolve, reject) => {
      const request = http.request({hostname: '127.0.0.1', port: this.port, path: route, method,
        headers: {Authorization: `Bearer ${this.key}`, 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(raw)}}, response => {
        const chunks = []; let count = 0;
        response.on('data', chunk => {count += chunk.length; if (count > 2 << 20) request.destroy(new Error('bounded response')); else chunks.push(chunk);});
        response.on('error', () => {clearTimeout(timer); reject(new Error('lab API transport failed'));});
        response.on('end', () => {
          clearTimeout(timer);
          if (response.statusCode < 200 || response.statusCode >= 300) return reject(new Error(`lab API HTTP ${response.statusCode}`));
          try {resolve(JSON.parse(Buffer.concat(chunks).toString()));} catch {reject(new Error('lab API JSON invalid'));}
        });
      });
      const timer = setTimeout(() => request.destroy(new Error('bounded deadline')), 45000);
      request.on('error', () => {clearTimeout(timer); reject(new Error('lab API transport failed'));});
      request.end(raw);
    });
    this.pod(this.apiPod.metadata.name, this.apiPod.metadata.uid);
    return result;
  }
  async code(id, language, code) {
    const result = await this.api('POST', `/api/v1/sandboxes/${id}/exec`, {language, code, timeout: 30});
    check(result.exit_code === 0, 'sandbox execution failed');
    return result.stdout;
  }
  async pristine(fuse) {
    return waitFor(() => this.objects('pods', 'sandbox.managed=true'), pods => {
      const candidate = pods.find(pod => (fuse || ready(pod)) && !pod.metadata.deletionTimestamp &&
        (pod.metadata.labels?.['sandbox.workspace.mode'] === 'fuse') === fuse &&
        (fuse ? pod.metadata.labels?.['sandbox.pool.state'] === 'prepared' : pod.metadata.labels?.['sandbox.pool'] === 'true'));
      if (!candidate) return false;
      assertPodIdentity(candidate, this.expected, candidate.metadata.uid);
      return candidate;
    }, 'pristine pool', 120000).then(pods => pods.find(pod => (fuse || ready(pod)) && !pod.metadata.deletionTimestamp &&
      (pod.metadata.labels?.['sandbox.workspace.mode'] === 'fuse') === fuse &&
      (fuse ? pod.metadata.labels?.['sandbox.pool.state'] === 'prepared' : pod.metadata.labels?.['sandbox.pool'] === 'true')));
  }
  async createSandbox(fuse) {
    const pristine = await this.pristine(fuse);
    if (fuse) {
      const health = JSON.parse(this.exec(pristine, 'workspace-mounter', ['/usr/local/bin/workspace-mounter', 'health', 'prepared']));
      check(health.version === 1 && health.state === 'prepared' && health.runtime_uid === pristine.metadata.uid &&
        health.generation === 0 && health.mount_type === '' && !health.restart_detected && health.cache_bytes === 0,
      'pristine exact UID/generation mismatch');
    }
    const result = await this.api('POST', '/api/v1/sandboxes', {mode: fuse ? 'persistent' : 'ephemeral', timeout: 300,
      ...(fuse ? {workspace_path: 'api-workspace', workspace_mount_mode: 'fuse'} : {})});
    check(result.id, 'API sandbox identity missing');
    this.active.add(result.id);
    if (fuse) this.hadFuse = true;
    check(result.runtime_id === pristine.metadata.name, 'API did not acquire pristine pool Pod');
    result.pod = this.pod(result.runtime_id, pristine.metadata.uid);
    result.labFuse = fuse;
    this.verifyImages(result.pod, fuse ? {sandbox: IMAGES.runtime, 'workspace-mounter': IMAGES.mounter} : {sandbox: IMAGES.runtime});
    return result;
  }
  async destroySandbox(sandbox) {
    await this.api('DELETE', `/api/v1/sandboxes/${sandbox.id}`);
    this.active.delete(sandbox.id);
    await waitFor(() => this.objects('pods'), pods => !pods.some(pod => pod.metadata.uid === sandbox.pod.metadata.uid), 'exact sandbox termination', 60000);
    const policies = [...this.objects('networkpolicies'), ...this.objects('ciliumnetworkpolicies')];
    check(!policies.some(policy => JSON.stringify(policy.metadata.labels || {}).includes(sandbox.id) ||
      Object.values(policy.metadata.annotations || {}).includes(sandbox.pod.metadata.uid)), 'sandbox policy cleanup incomplete');
    await this.pristine(sandbox.labFuse);
  }
  async ordinary() {
    const sandbox = await this.createSandbox(false);
    for (const [language, code] of [['python', 'print("ordinary-python")'], ['nodejs', 'console.log("ordinary-node")'], ['bash', 'printf ordinary-bash']]) {
      check((await this.code(sandbox.id, language, code)).trim() === `ordinary-${language === 'nodejs' ? 'node' : language}`, 'ordinary language content mismatch');
    }
    const probe = JSON.parse(await this.code(sandbox.id, 'python', `import os,json\ns=open('/proc/self/status').read()\np='/workspace/ordinary.txt'\nf=open(p,'w');f.write('ordinary-verified');f.flush();os.fsync(f.fileno());f.close()\nassert open(p).read()=='ordinary-verified'\nprint(json.dumps({'uid':os.getuid(),'nnp':'NoNewPrivs:\\t1' in s,'seccomp':'Seccomp:\\t2' in s,'profile':open('/proc/self/attr/current').read().strip()}))`));
    check(probe.uid === 1000 && probe.nnp && probe.seccomp && probe.profile.endsWith('(enforce)') && !probe.profile.includes('unconfined'), 'ordinary security state mismatch');
    await this.destroySandbox(sandbox);
    this.stage('ordinary-API-languages-security-write-destroy-PASS', {runtimeUID: sandbox.pod.metadata.uid, profile: probe.profile});
  }
  observe(sandbox) {
    const health = JSON.parse(this.exec(sandbox.pod, 'workspace-mounter', ['/usr/local/bin/workspace-mounter', 'health', 'ready']));
    check(health.version === 1 && health.state === 'ready' && health.mount_type === 'fuse' &&
      health.runtime_uid === sandbox.pod.metadata.uid && health.generation === 1 && !health.restart_detected && !health.cache_exceeded,
    'exact mounter UID/generation health mismatch');
    const output = this.exec(sandbox.pod, 'workspace-mounter', ['/bin/sh', '-c',
      'cat /proc/1/attr/current; for p in /proc/[0-9]*; do IFS= read -r s < "$p/stat" || continue; case "$s" in *"(s3fs)"*) printf "%s\\n" "$s"; cat "$p/attr/current";; esac; done; cat /proc/1/mountinfo']);
    const lines = output.split('\n');
    check(lines[0] === `${PROFILE} (enforce)`, 'supervisor enforce mismatch');
    const statIndex = lines.findIndex(line => /^\d+ \(s3fs\)/.test(line));
    check(statIndex >= 0 && lines[statIndex + 1] === `${PROFILE} (enforce)`, 'real s3fs enforce mismatch');
    const stat = lines[statIndex].match(/^(\d+) \(s3fs\) (.*)$/);
    const fields = stat[2].split(' ');
    check(fields[1] === '1' && Number(fields[19]) > 0, 'real s3fs process ancestry invalid');
    const mount = lines.find(line => line.includes(' /workspace ') && line.includes(' - fuse.s3fs '));
    check(mount, 'actual workspace fuse.s3fs mount missing');
    return {pid: Number(stat[1]), startTime: fields[19], mountID: Number(mount.split(' ')[0]), generation: health.generation};
  }
  async object(storage, name, expectedStatus = 200, method = 'GET') {
    check(['GET', 'DELETE'].includes(method) && ['accepted.txt', 'final.txt', 'remove.txt'].includes(name) &&
      /^[a-f0-9]{8}$/.test(this.expected.run) && /^ds-ai-worker-[1-5]$/.test(this.expected.node) && storage.useSSL === true,
    'object operation escaped lab scope');
    const keyPath = `${storage.bucket}/validation/apparmor-all-nodes/${this.expected.run}/${this.expected.node}/api-workspace/${name}`;
    const url = new URL(`https://${storage.endpoint.replace(/^https:\/\//, '').replace(/\/$/, '')}/${keyPath}`);
    const date = new Date().toISOString().replace(/[:-]|\.\d{3}/g, '');
    const day = date.slice(0, 8), payload = hash(''), region = storage.region || 'us-east-1';
    const scope = `${day}/${region}/s3/aws4_request`, signed = 'host;x-amz-content-sha256;x-amz-date';
    const canonical = `${method}\n${url.pathname}\n\nhost:${url.host}\nx-amz-content-sha256:${payload}\nx-amz-date:${date}\n\n${signed}\n${payload}`;
    const hmac = (key, text) => crypto.createHmac('sha256', key).update(text).digest();
    const signingKey = hmac(hmac(hmac(hmac(`AWS4${storage.secretKey}`, day), region), 's3'), 'aws4_request');
    const signature = hmac(signingKey, `AWS4-HMAC-SHA256\n${date}\n${scope}\n${hash(canonical)}`).toString('hex');
    return new Promise((resolve, reject) => {
      const request = https.request(url, {rejectUnauthorized: true, method, headers: {
        'x-amz-content-sha256': payload, 'x-amz-date': date,
        Authorization: `AWS4-HMAC-SHA256 Credential=${storage.accessKey}/${scope}, SignedHeaders=${signed}, Signature=${signature}`,
      }}, response => {
        const chunks = []; let count = 0;
        response.on('data', chunk => {count += chunk.length; if (count > 65536) request.destroy(new Error('bounded response')); else chunks.push(chunk);});
        response.on('error', () => {clearTimeout(timer); reject(new Error('lab authenticated TLS object request failed'));});
        response.on('end', () => {clearTimeout(timer); response.statusCode === expectedStatus ? resolve(Buffer.concat(chunks).toString()) : reject(new Error(`lab object HTTP ${response.statusCode}`));});
      });
      const timer = setTimeout(() => request.destroy(new Error('bounded deadline')), 15000);
      request.on('error', () => {clearTimeout(timer); reject(new Error('lab authenticated TLS object request failed'));});
      request.end();
    });
  }
  async negatives(sandbox) {
    const marker = `aa-api-${this.expected.run}`;
    // Use the same kernel-log time domain, not /proc/uptime (observed ~82s
    // offset on worker-1). An unpredictable run marker plus this pre-operation
    // log watermark/PID prevents old or other-workload events being accepted.
    const since = kernelAuditWatermark(this.exec(this.loader, 'apparmor-loader', ['/bin/dmesg']));
    this.exec(this.loader, 'apparmor-loader', ['/bin/sh', '-c',
      `set -e; cat /etc/shadow >/dev/null; printf control >/dev/shm/${marker}-control; rm /dev/shm/${marker}-control; mkdir /run/apparmor-loader/${marker}; mount -t tmpfs -o size=1m tmpfs /run/apparmor-loader/${marker}; umount /run/apparmor-loader/${marker}; rmdir /run/apparmor-loader/${marker}`]);
    this.exec(sandbox.pod, 'workspace-mounter', ['/bin/sh', '-c', `cat /etc/hosts >/dev/null && printf allowed >/run/s3fs/${marker}-allowed`]);
    this.exec(sandbox.pod, 'workspace-mounter', ['/bin/sh', '-c', `printf denied >/dev/shm/${marker}-shadow; exec /bin/cat /etc/shadow`], undefined, true);
    this.exec(sandbox.pod, 'workspace-mounter', ['/bin/sh', '-c', `printf denied >/dev/shm/${marker}-exec; exec /bin/sh -c true`], undefined, true);
    this.exec(sandbox.pod, 'workspace-mounter', ['/bin/mkdir', `/var/cache/s3fs/${marker}-mount`]);
    this.exec(sandbox.pod, 'workspace-mounter', ['/bin/mount', '-t', 'tmpfs', 'tmpfs', `/var/cache/s3fs/${marker}-mount`], undefined, true);
    const audit = this.exec(this.loader, 'apparmor-loader', ['/bin/dmesg']);
    const ownLines = audit.split('\n').filter(line => line.includes(`profile="${PROFILE}"`) && line.includes(marker));
    const pids = ownLines.map(line => line.match(/\bpid=(\d+)\b/)?.[1]).filter(Boolean);
    const diagnostics = audit.split('\n').filter(line => line.includes(`profile="${PROFILE}"`) &&
      (line.includes(marker) || pids.includes(line.match(/\bpid=(\d+)\b/)?.[1]) && /name="(\/etc\/shadow|\/(usr\/)?bin\/(dash|sh))"/.test(line)))
      .slice(-40).map(line => ({time: Number(line.match(/^\[\s*([\d.]+)\]/)?.[1]),
        operation: line.match(/operation="([a-z_]+)"/)?.[1], name: line.match(/\bname="([^"]+)"/)?.[1], pid: line.match(/\bpid=(\d+)\b/)?.[1]}));
    this.stage('own-negative-audit-observations', {since, evidence: selectAuditEvidence(audit, PROFILE, marker, since), diagnostics});
    const readEvidence = () => selectAuditEvidence(this.exec(this.loader, 'apparmor-loader', ['/bin/dmesg']), PROFILE, marker, since);
    let evidence = readEvidence();
    // kauditd printk is rate-limited globally. Retry only missing own negative
    // cases, spaced beyond its normal refill window; never change host knobs.
    const until = Date.now() + 120000;
    for (let round = 0; round < 4 && !Object.values(evidence).every(Boolean); round++) {
      for (const [missing, argv] of [
        [!evidence.shadow || !evidence.write, ['/bin/sh', '-c', `printf denied >/dev/shm/${marker}-shadow; exec /bin/cat /etc/shadow`]],
        [!evidence.exec, ['/bin/sh', '-c', `printf denied >/dev/shm/${marker}-exec; exec /bin/sh -c true`]],
        [!evidence.mount, ['/bin/mount', '-t', 'tmpfs', 'tmpfs', `/var/cache/s3fs/${marker}-mount`]],
      ]) {
        if (!missing || Date.now() + 6500 >= until) continue;
        await sleep(6500);
        this.exec(sandbox.pod, 'workspace-mounter', argv, undefined, true);
        evidence = readEvidence();
      }
      this.stage('bounded-missing-audit-retry', {round: round + 1, evidence});
    }
    check(Object.values(evidence).every(Boolean), 'kernel correlated DENIED incomplete after bounded retries');
    this.exec(sandbox.pod, 'workspace-mounter', ['/bin/rmdir', `/var/cache/s3fs/${marker}-mount`]);
    this.stage('AppArmor-allowed-denied-correlated-kernel-PASS', evidence);
  }
  async fuse(storage) {
    const sandbox = await this.createSandbox(true);
    const observed = this.observe(sandbox);
    this.stage('actual-s3fs-UID-generation-profile-mount-verified', {runtimeUID: sandbox.pod.metadata.uid, ...observed, profile: PROFILE});
    await this.code(sandbox.id, 'python', `import os\np='/workspace/accepted.txt'\nf=open(p,'w');f.write('first');f.flush();os.fsync(f.fileno());f.close()\nassert open(p).read()=='first'\nf=open(p,'w');f.write('verified');f.flush();os.fsync(f.fileno());f.close()\nf=open(p,'a');f.write('-append');f.flush();os.fsync(f.fileno());f.close()\nos.rename(p,'/workspace/final.txt')\nassert open('/workspace/final.txt').read()=='verified-append'\nf=open('/workspace/remove.txt','w');f.write('delete');f.close();os.unlink('/workspace/remove.txt')`);
    await this.api('POST', `/api/v1/sandboxes/${sandbox.id}/workspace/sync`, {direction: 'from_container'});
    const info = await this.api('GET', `/api/v1/sandboxes/${sandbox.id}/workspace/info`);
    check(info.mounted && info.mount_type === 'fuse' && info.flushed, 'FUSE flush publication invalid');
    check(await this.object(storage, 'final.txt') === 'verified-append', 'independent persisted content mismatch');
    this.stage('FUSE-API-fsync-flush-authenticated-TLS-object-verified');
    await this.negatives(sandbox);
    check(JSON.stringify(this.observe(sandbox)) === JSON.stringify(observed), 's3fs identity/mount changed during negative tests');
    await this.code(sandbox.id, 'python', `import os\nassert open('/workspace/final.txt').read()=='verified-append'\nos.unlink('/workspace/final.txt')`);
    await this.api('POST', `/api/v1/sandboxes/${sandbox.id}/workspace/sync`, {direction: 'from_container'});
    await this.object(storage, 'final.txt', 404);
    await this.destroySandbox(sandbox);
    this.stage('FUSE-API-pristine-fsync-flush-TLS-object-destroy-PASS', {runtimeUID: sandbox.pod.metadata.uid, ...observed, profile: PROFILE});
  }
  async recreateLoader() {
    this.pod(this.loader.metadata.name, this.loader.metadata.uid);
    this.deleteExact(`/api/v1/namespaces/${this.expected.name}/pods/${this.loader.metadata.name}`, this.loader.metadata.uid);
    const pods = await waitFor(() => this.objects('pods', `app=sandbox-apparmor-loader,release=${this.release}`),
      pods => pods.some(pod => pod.metadata.uid !== this.loader.metadata.uid && ready(pod)), 'loader replacement', 120000);
    const replacement = pods.find(pod => pod.metadata.uid !== this.loader.metadata.uid && ready(pod));
    this.pod(replacement.metadata.name, replacement.metadata.uid);
    this.kernelProfile(replacement);
    this.stage('own-loader-Pod-recreation-enforce-PASS', {oldUID: this.loader.metadata.uid, newUID: replacement.metadata.uid});
    this.loader = replacement;
  }
  deleteExact(uri, uid) {
    this.namespace();
    const namespaceURI = `/api/v1/namespaces/${this.expected.name}`;
    if (uri === namespaceURI) check(uid === this.expected.uid, 'namespace deletion identity mismatch');
    else {
      check(uri.startsWith(`${namespaceURI}/pods/`) && /^[a-z0-9-]+$/.test(uri.slice(`${namespaceURI}/pods/`.length)), 'deletion escaped lab scope');
      this.pod(uri.slice(`${namespaceURI}/pods/`.length), uid);
    }
    kube(['delete', '--raw', uri, '-f', '-'], JSON.stringify({apiVersion: 'v1', kind: 'DeleteOptions', preconditions: {uid}}));
  }
  async cleanup() {
    if (!this.expected.uid) return;
    this.namespace();
    for (const id of this.active) await this.api('DELETE', `/api/v1/sandboxes/${id}`);
    this.active.clear();
    if (this.resourcesCreated) {
      const jobs = this.render(['pre-delete-drain']);
      this.create(jobs);
      await waitFor(() => this.objects('jobs'), jobs => {
        check(!jobs.some(job => job.status.conditions?.some(condition => condition.type === 'Failed' && condition.status === 'True')), 'lab drain failed');
        return jobs.some(job => job.status.conditions?.some(condition => condition.type === 'Complete' && condition.status === 'True'));
      }, 'release drain', 260000);
    }
    check(this.objects('pods', 'sandbox.managed=true').length === 0, 'lab managed runtime cleanup incomplete');
    check(this.objects('networkpolicies', 'sandbox.managed=true').length === 0 &&
      this.objects('ciliumnetworkpolicies', 'sandbox.managed=true').length === 0, 'lab managed policy cleanup incomplete');
    this.stage('normal-release-drain-zero-managed-state-PASS');
    if (this.hadFuse) {
      for (const name of ['accepted.txt', 'final.txt', 'remove.txt']) {
        await this.object(this.storage, name, 204, 'DELETE');
        await this.object(this.storage, name, 404);
      }
      this.stage('exact-test-files-object-cleanup-PASS');
    }
    this.deleteExact(`/api/v1/namespaces/${this.expected.name}`, this.expected.uid);
    await waitFor(() => get('namespaces'), namespaces => !namespaces.items.some(namespace => namespace.metadata.name === this.expected.name), 'namespace cleanup', 120000);
    const inventory = `${this.expected.name}-${this.release}-network-inventory`;
    await waitFor(() => [get('clusterroles'), get('clusterrolebindings')], groups => !groups.some(group => group.items.some(object => object.metadata.name === inventory)), 'inventory RBAC garbage collection', 60000);
    this.report.cleaned = true;
    this.stage('namespace-inventory-RBAC-cleanup-PASS');
  }
  stopForward() { if (this.forward && this.forward.exitCode === null) this.forward.kill('SIGTERM'); }
  clearPrivateFiles() {
    const file = path.join(this.dir, 'values.json');
    if (fs.existsSync(file)) fs.unlinkSync(file);
    this.key = '';
  }
}

async function main(args) {
  const argument = name => args.indexOf(name) >= 0 ? args[args.indexOf(name) + 1] : undefined;
  const options = {execute: args.includes('--execute'), context: argument('--context'), node: argument('--node')};
  if (!options.execute) {console.log('Dry run: no Secret reads or Kubernetes writes. Use --execute --context ds-ai-research --node ds-ai-worker-N.'); return;}
  validateOptions(options);
  check(hash(fs.readFileSync(path.join(CHART, 'files/apparmor/workspace-mounter.profile'), 'utf8').replace(/\r\n/g, '\n').trim() + '\n') === DIGEST, 'Chart immutable profile changed');
  const node = get('node', options.node);
  check(ready(node) && node.status.nodeInfo.architecture === 'arm64', 'target node unavailable or not arm64');
  const baseline = businessSnapshot();
  const storage = privateStorageConfiguration();
  const lab = new Lab(options.node);
  console.log(`Non-sensitive report: ${path.join(lab.dir, 'report.json')}`);
  let failure;
  try {
    await lab.setup(storage);
    await lab.ordinary();
    await lab.fuse(storage);
    await lab.recreateLoader();
  } catch (error) {failure = error; lab.stage('FAILED', {error: error.message});}
  try {await lab.cleanup();} catch (error) {failure ||= error; lab.stage('CLEANUP-NOT-CONFIRMED', {error: error.message});}
  finally {lab.stopForward(); lab.clearPrivateFiles(); storage.accessKey = ''; storage.secretKey = '';}
  check(JSON.stringify(businessSnapshot()) === JSON.stringify(baseline), 'business workload baseline changed during lab');
  lab.stage('business-UID-template-image-restarts-health-unchanged-PASS');
  if (failure) throw failure;
  lab.stage('ALL-NODE-CASE-PASS');
}
module.exports = {assertNamespaceIdentity, assertPodIdentity, validateOptions, buildLabValues, selectAuditEvidence, kernelAuditWatermark, Lab};
if (require.main === module) main(process.argv.slice(2)).catch(error => {console.error(error.message); process.exitCode = 1;});
