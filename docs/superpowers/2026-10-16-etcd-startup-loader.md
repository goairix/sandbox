# etcd mounted deployment startup check

Run `sandbox --check-etcd-startup /etc/sandbox/deployment.json` to load mounted credentials and construct the existing etcd Backend and ordinary cold-create Service. The command closes both resources before exiting: success is exit 0, failure is nonzero. The check has a 30-second total deadline, starts no HTTP listener, and requires no legacy configuration or Redis. It rejects `--config`, positional arguments, and every explicitly supplied deployment/drain/audit/recovery flag, including flags set to their default values.

This command checks an existing deployment. Operators must separately provision the exact etcd identity/restore metadata and complete preparation inventory for the configured quotas. The loader never creates identity, issues authority certificates, initializes inventory, grants a lease, or creates a Pod. This step does not switch the ordinary HTTP server from Redis to etcd.

The manifest below uses relative mounted file paths. Replace identity values with the existing operator values, the image with the pinned image matching the launcher's contract, and resources with the fixed deployment profile. `quota_slots` has exactly 256 entries; partition 7 has one slot in this example. It must match the inventory that was provisioned, including all zero partitions.

```json
{
  "version": 1,
  "etcd": {
    "endpoints": ["https://etcd.internal:2379"],
    "identity": {
      "schema_version": 1,
      "prefix": "/sandbox",
      "authority_id": "production",
      "cell": "cell-a",
      "cluster_id": 123456789,
      "storage_id": "storage-a",
      "runtime_id": "launcher-a",
      "restore_epoch": "restore-a"
    },
    "tls": {
      "ca_file": "etcd/ca.pem",
      "cert_file": "etcd/client.pem",
      "key_file": "etcd/client-key.pem",
      "server_name": "etcd.internal"
    },
    "dial_timeout": "2s",
    "request_timeout": "2s",
    "snapshot_cache_bytes": 262144
  },
  "preparation": {
    "quota_partition": 7,
    "quota_slots": [
      0,0,0,0,0,0,0,1,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,
      0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0
    ]
  },
  "launcher_config_file": "launcher.json",
  "root_private_key_file": "signing/root-key.pem",
  "issuer_private_key_file": "signing/issuer-key.pem",
  "issuer_certificate_file": "signing/issuer.json",
  "kubernetes": {
    "kubeconfig_file": "kubernetes/config",
    "namespace": "runtimes",
    "max_active": 4,
    "request_timeout": "1s"
  },
  "image": "registry.example/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000000",
  "node_name": "worker-a",
  "worker_id": "api-a",
  "resources": {"cpu":"2","memory":"512Mi","disk":"1Mi","tmp_disk":"50Mi"},
  "business": {
    "principal": "sandbox-api",
    "defaults": {
      "timeout_seconds": 60,
      "resources": {"cpu":"2","memory":"512Mi","disk":"1Mi","tmp_disk":"50Mi"}
    }
  }
}
```

`launcher.json` is the existing canonical protected launcher v2 format, with no extra whitespace or fields. It supplies the single trust binding, roots, contract digest, node UID/boot, and fixed signed-clock socket/public key/audience. Its binding must match the exact namespace, runtime authority and restore epoch. A real operator-pinned signed-clock server must be reachable at that socket; there is no local time provider fallback. Root and issuer private keys are PKCS8 `PRIVATE KEY` PEM containing Ed25519 keys. The original service validates root membership and the issuer key/certificate binding and lifetime.

The etcd connection requires verified mutual TLS 1.3. Kubeconfig requires a static HTTPS server with verification enabled. Only its explicit current context is selected. Relative CA, client certificate/key and token paths resolve relative to kubeconfig. The loader reads selected credentials into memory and clears the path fields before passing the configuration to the original static Kubernetes clients. Inline credential data takes precedence over the corresponding file path. Exec plugins and auth-provider plugins are rejected even in unused user entries; proxy, impersonation and transport extensions are unsupported. Environment kubeconfig search paths and in-cluster fallback are never used.

Manifest and kubeconfig are at most 64 KiB; each TLS file is at most 64 KiB, launcher 16384 bytes, issuer certificate 8192 bytes, signing private key 4096 bytes and token file 16384 bytes. Actual reads are bounded and reject directories, devices and pipes; projected Secret symlinks are supported. Keep credential mounts restricted to the deployment account. Errors identify the failed configuration stage without printing raw credentials.

Library callers can use `deployment.Open(ctx, path)` and borrow `Service()` / `Backend()` for the deployment lifetime. Open also constructs an owned start/renew Exec signer from the existing mounted issuer material; constructing it does not register metadata. Registration remains lazy in the first actual PrepareExecEffect. Close the returned Deployment: Close is repeatable and closes/joins the Service, then the Backend, then clears the owned signer key. Construction failures reclaim every already-open resource. A separate call to the service Exec method creates a new operation; RequestID is attribution, not a deduplication/recovery key, so unknown results must not be automatically retried.

The limited etcd account must be able to perform authenticated endpoint Status as well as the configured namespace operations. The v3.6.5 native fixture failed this startup check because its Status handler requires administrator permission; v3.6.14 instead checks authentication. The Exec integration fixture uses official v3.6.14, matching the client dependency; it does not grant the namespace account root or bypass startup cluster verification. This is a verified version distinction, not a claim that every intermediate version is compatible. Compare the upstream [v3.6.5 handler](https://raw.githubusercontent.com/etcd-io/etcd/v3.6.5/server/etcdserver/api/v3rpc/maintenance.go) and [v3.6.14 handler](https://raw.githubusercontent.com/etcd-io/etcd/v3.6.14/server/etcdserver/api/v3rpc/maintenance.go).
