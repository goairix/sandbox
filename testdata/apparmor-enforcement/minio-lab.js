// 仅供本次指定 namespace 的隔离验收；不读取任何业务 Secret。
const crypto = require('crypto');
const https = require('https');
const {spawn, spawnSync} = require('child_process');
const ns = 'sandbox-apparmor-enforcement-20260914-a18c24';
const context = 'ds-ai-research';
const dns = `minio.${ns}.svc.cluster.local`;
const bucket = 'apparmor-isolated-test';
// 固定、公开的实验凭据，绝不可用于生产。
const access = 'apparmor-lab-only';
const secret = 'apparmor-lab-only-not-production-password';
const kubectl = ['--context', context, '--namespace', ns];

function command(args, input) {
  const result = spawnSync('kubectl', [...kubectl, ...args], {input, encoding: 'utf8', maxBuffer: 4 << 20, timeout: 30000});
  if (result.status !== 0) throw new Error('lab kubectl operation failed');
  return result.stdout;
}

async function certificate() {
  const {privateKey} = crypto.generateKeyPairSync('ec', {
    namedCurve: 'prime256v1',
    privateKeyEncoding: {type: 'pkcs8', format: 'pem'},
    publicKeyEncoding: {type: 'spki', format: 'pem'},
  });
  const config = `[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=apparmor-isolated-minio\n[ext]\nbasicConstraints=critical,CA:TRUE\nsubjectAltName=DNS:${dns},DNS:localhost,IP:127.0.0.1\n`;
  const cert = await new Promise((resolve, reject) => {
    const process = spawn('openssl', ['req', '-new', '-x509', '-sha256', '-days', '1', '-key', '/dev/stdin', '-config', '/dev/fd/3'], {stdio: ['pipe', 'pipe', 'pipe', 'pipe']});
    let output = '';
    process.stdout.on('data', data => output += data);
    process.on('error', () => reject(new Error('lab certificate process unavailable')));
    process.on('close', code => code === 0 ? resolve(output) : reject(new Error('lab certificate generation failed')));
    process.stdin.end(privateKey);
    process.stdio[3].end(config);
  });
  return {cert, privateKey};
}

function assertNamespace() {
  const namespace = JSON.parse(command(['get', 'namespace', ns, '-o', 'json']));
  if (namespace.metadata.uid !== 'd88ea2e2-c80d-4bdb-bc77-50bf2bfb6f55' || namespace.metadata.labels['sandbox-test-run'] !== 'a18c24') throw new Error('lab namespace identity mismatch');
}

async function setup() {
  const {cert, privateKey} = await certificate();
  const meta = name => ({name, namespace: ns, labels: {'sandbox-test-run': 'a18c24'}});
  const items = [
    {apiVersion: 'v1', kind: 'Secret', metadata: meta('minio-isolated-tls'), type: 'Opaque', stringData: {'public.crt': cert, 'private.key': privateKey}},
    {apiVersion: 'v1', kind: 'ConfigMap', metadata: meta('minio-isolated-ca'), data: {'public.crt': cert}},
    {apiVersion: 'v1', kind: 'Service', metadata: meta('minio'), spec: {selector: {app: 'apparmor-lab-minio'}, ports: [{port: 9000, targetPort: 9000}]}},
    {apiVersion: 'v1', kind: 'Pod', metadata: {...meta('minio'), labels: {...meta('minio').labels, app: 'apparmor-lab-minio'}}, spec: {
      nodeSelector: {'kubernetes.io/hostname': 'ds-ai-worker-2', 'kubernetes.io/os': 'linux'},
      automountServiceAccountToken: false, enableServiceLinks: false, restartPolicy: 'Never', activeDeadlineSeconds: 1200,
      securityContext: {seccompProfile: {type: 'RuntimeDefault'}},
      containers: [{name: 'minio', image: 'quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z@sha256:54d3d6a0a58fb25b4e9943d1db3828d3b4de44666f911381b4fda57175488194',
        command: ['minio', 'server', '/data', '--certs-dir', '/run/minio-tls', '--address', ':9000'],
        env: [{name: 'MINIO_ROOT_USER', value: access}, {name: 'MINIO_ROOT_PASSWORD', value: secret}],
        securityContext: {runAsUser: 0, readOnlyRootFilesystem: true, allowPrivilegeEscalation: false, capabilities: {drop: ['ALL']}},
        resources: {requests: {cpu: '50m', memory: '128Mi'}, limits: {cpu: '500m', memory: '512Mi'}},
        readinessProbe: {httpGet: {path: '/minio/health/ready', port: 9000, scheme: 'HTTPS'}, periodSeconds: 2},
        volumeMounts: [{name: 'data', mountPath: '/data'}, {name: 'tls', mountPath: '/run/minio-tls', readOnly: true}, {name: 'config', mountPath: '/root/.minio'}, {name: 'tmp', mountPath: '/tmp'}]}],
      volumes: [{name: 'data', emptyDir: {sizeLimit: '256Mi'}}, {name: 'tls', secret: {secretName: 'minio-isolated-tls', defaultMode: 256}}, {name: 'config', emptyDir: {}}, {name: 'tmp', emptyDir: {sizeLimit: '64Mi'}}],
    }},
    {apiVersion: 'networking.k8s.io/v1', kind: 'NetworkPolicy', metadata: meta('lab-only-network'), spec: {
      podSelector: {}, policyTypes: ['Ingress', 'Egress'],
      ingress: [{from: [{podSelector: {}}]}],
      egress: [{to: [{podSelector: {}}]}, {to: [{namespaceSelector: {matchLabels: {'kubernetes.io/metadata.name': 'kube-system'}}}], ports: [{protocol: 'UDP', port: 53}, {protocol: 'TCP', port: 53}]}],
    }},
  ];
  command(['create', '-f', '-'], JSON.stringify({apiVersion: 'v1', kind: 'List', items}));
  console.log('isolated TLS MinIO resources created (no PVC, no business credentials)');
}

const hash = value => crypto.createHash('sha256').update(value).digest('hex');
const hmac = (key, value) => crypto.createHmac('sha256', key).update(value).digest();
async function requestObject(method, path, body, ca) {
  const date = new Date().toISOString().replace(/[:-]|\.\d{3}/g, '');
  const day = date.slice(0, 8);
  const payloadHash = hash(body);
  const headers = `host:127.0.0.1:19000\nx-amz-content-sha256:${payloadHash}\nx-amz-date:${date}\n`;
  const signed = 'host;x-amz-content-sha256;x-amz-date';
  const scope = `${day}/us-east-1/s3/aws4_request`;
  const canonical = `${method}\n${path}\n\n${headers}\n${signed}\n${payloadHash}`;
  const toSign = `AWS4-HMAC-SHA256\n${date}\n${scope}\n${hash(canonical)}`;
  const key = hmac(hmac(hmac(hmac('AWS4' + secret, day), 'us-east-1'), 's3'), 'aws4_request');
  const signature = hmac(key, toSign).toString('hex');
  return new Promise((resolve, reject) => {
    const request = https.request({hostname: '127.0.0.1', port: 19000, method, path, ca, rejectUnauthorized: true, timeout: 10000,
      headers: {'x-amz-date': date, 'x-amz-content-sha256': payloadHash, 'content-length': Buffer.byteLength(body), Authorization: `AWS4-HMAC-SHA256 Credential=${access}/${scope}, SignedHeaders=${signed}, Signature=${signature}`}}, response => {
      const chunks = [];
      let size = 0;
      response.on('data', chunk => {
        size += chunk.length;
        if (size > 65536) request.destroy(new Error('lab S3 response too large'));
        else chunks.push(chunk);
      });
      response.on('end', () => response.statusCode >= 200 && response.statusCode < 300 ? resolve(Buffer.concat(chunks)) : reject(new Error('lab S3 ' + method + ' HTTP ' + response.statusCode)));
    });
    request.on('error', () => reject(new Error('lab S3 connection failed')));
    request.on('timeout', () => request.destroy());
    request.end(body);
  });
}
const put = (path, body, ca) => requestObject('PUT', path, body, ca);

async function seed() {
  const ca = command(['get', 'configmap', 'minio-isolated-ca', '-o', 'jsonpath={.data.public\\.crt}']);
  await put('/' + bucket, '', ca);
  await put('/' + bucket + '/workspace-test/', '', ca);
  await put('/' + bucket + '/workspace-test/.seed', 'apparmor-lab-seed', ca);
  console.log('isolated bucket and prefix seeded over verified TLS');
}

async function seedPrefix() {
  const ca = command(['get', 'configmap', 'minio-isolated-ca', '-o', 'jsonpath={.data.public\\.crt}']);
  await put('/' + bucket + '/workspace-test/', '', ca);
  console.log('isolated canonical directory marker created');
}

async function verify() {
  const ca = command(['get', 'configmap', 'minio-isolated-ca', '-o', 'jsonpath={.data.public\\.crt}']);
  const body = await requestObject('GET', '/' + bucket + '/workspace-test/accepted.txt', '', ca);
  if (body.toString() !== 'apparmor-live-persistence-20260914\n') throw new Error('persisted object content mismatch');
  console.log('post-unmount object content verified over authenticated TLS');
}

const mode = process.argv[2];
const modes = {setup, seed, 'seed-prefix': seedPrefix, verify};
if (!Object.hasOwn(modes, mode)) throw new Error('usage: node minio-lab.js {setup|seed|seed-prefix|verify}');
Promise.resolve().then(() => {assertNamespace(); return modes[mode]();}).catch(error => {console.error(error.message); process.exitCode = 1;});
