{{- define "sandbox.backend.provider" -}}
{{- if eq .Values.config.storage.filesystem.preset "minio" -}}minio
{{- else if or (eq .Values.config.storage.filesystem.preset "huawei-obs-public") (eq .Values.config.storage.filesystem.preset "huawei-obs-private") -}}obs
{{- else -}}{{ fail (printf "unsupported config.storage.filesystem.preset %q" .Values.config.storage.filesystem.preset) }}
{{- end -}}

{{- end -}}

{{- define "sandbox.metadataPrefix" -}}goairix.github.io{{- end -}}

{{- define "sandbox.backend.profile" -}}
{{- if eq .Values.config.storage.filesystem.preset "minio" -}}minio-sigv4-path-style-v1
{{- else if eq .Values.config.storage.filesystem.preset "huawei-obs-public" -}}huawei-obs-public-v1
{{- else if eq .Values.config.storage.filesystem.preset "huawei-obs-private" -}}huawei-obs-private-2023-v1
{{- else -}}{{ fail (printf "unsupported config.storage.filesystem.preset %q" .Values.config.storage.filesystem.preset) }}
{{- end -}}
{{- end -}}

{{- define "sandbox.fuseEnabled" -}}
{{- if has "fuse" .Values.config.workspace.enabledMountModes -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "sandbox.apparmor.digest" -}}
{{- $template := .Files.Get "files/apparmor/workspace-mounter.profile" | replace "\r\n" "\n" | trim -}}
{{- if empty $template -}}{{ fail "AppArmor profile template is missing" }}{{- end -}}
{{- printf "%s\n" $template | sha256sum -}}
{{- end -}}

{{- define "sandbox.effectiveLSMProfile" -}}
{{- if .Values.apparmorLoader.enabled -}}
{{- printf "sandbox-fuse-%s" (include "sandbox.apparmor.digest" .) -}}
{{- else -}}{{ .Values.config.workspace.lsmProfile | default "" }}{{- end -}}
{{- end -}}

{{- define "sandbox.effectiveNodeSelector" -}}
{{- $selector := .Values.config.runtime.kubernetes.nodeSelector | default dict -}}
{{- if not (kindIs "map" $selector) -}}{{ fail "config.runtime.kubernetes.nodeSelector must be a string map" }}{{- end -}}
{{- $selector = deepCopy $selector -}}
{{- range $key, $value := $selector -}}
{{- if not (kindIs "string" $value) -}}{{ fail "config.runtime.kubernetes.nodeSelector values must be strings" }}{{- end -}}
{{- end -}}
{{- if .Values.apparmorLoader.enabled -}}
{{- if and (hasKey $selector "kubernetes.io/os") (ne (get $selector "kubernetes.io/os") "linux") -}}
{{- fail "AppArmor loader requires kubernetes.io/os=linux" -}}
{{- end -}}
{{- $_ := set $selector "kubernetes.io/os" "linux" -}}
{{- end -}}
{{- if gt (len $selector) 64 -}}{{ fail "effective Kubernetes nodeSelector exceeds 64 entries" }}{{- end -}}
{{- $selector | toJson -}}
{{- end -}}

{{- define "sandbox.validateAppArmorLoader" -}}
{{- $_ := include "sandbox.effectiveNodeSelector" . -}}
{{- if .Values.apparmorLoader.enabled -}}
{{- if ne .Values.config.runtime.type "kubernetes" -}}{{ fail "AppArmor loader requires Kubernetes runtime" }}{{- end -}}
{{- if ne (include "sandbox.fuseEnabled" .) "true" -}}{{ fail "AppArmor loader requires FUSE" }}{{- end -}}
{{- if .Values.config.workspace.allowMissingLSMForKind -}}{{ fail "AppArmor loader forbids allowMissingLSMForKind" }}{{- end -}}
{{- if or (empty .Values.apparmorLoader.image.repository) (empty .Values.apparmorLoader.image.tag) -}}{{ fail "AppArmor loader image is required" }}{{- end -}}
{{- range $name, $value := dict "checkIntervalSeconds" .Values.apparmorLoader.checkIntervalSeconds "parserTimeoutSeconds" .Values.apparmorLoader.parserTimeoutSeconds "startupTimeoutSeconds" .Values.apparmorLoader.startupTimeoutSeconds -}}
{{- if or (lt (int $value) 1) (gt (int $value) 600) -}}{{ fail (printf "apparmorLoader.%s must be between 1 and 600" $name) }}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "sandbox.validateRedis" -}}
{{- if .Values.redis.enabled -}}
{{- if not (has .Values.redis.mode (list "standalone" "sentinel")) -}}{{ fail "redis.mode must be standalone or sentinel" }}{{- end -}}
{{- if eq .Values.redis.mode "sentinel" -}}
{{- if or (not (kindIs "bool" .Values.startupProbe.enabled)) (not .Values.startupProbe.enabled) -}}{{ fail "built-in Sentinel requires the API startup probe" }}{{- end -}}
{{- range $key := list "periodSeconds" "timeoutSeconds" "failureThreshold" -}}
{{- $value := index $.Values.startupProbe $key -}}
{{- $maximum := ternary 2147483647 60 (eq $key "failureThreshold") -}}
{{- if or (not (regexMatch "^[0-9]+$" (printf "%v" $value))) (lt (int64 $value) 1) (gt (int64 $value) (int64 $maximum)) -}}{{ fail "built-in Sentinel API startup probe requires bounded positive integers" }}{{- end -}}
{{- end -}}
{{- if lt (int .Values.redis.sentinel.failoverTimeoutMilliseconds) (mul 2 (int .Values.redis.sentinel.downAfterMilliseconds)) -}}{{ fail "Sentinel failover timeout must be at least twice down-after threshold" }}{{- end -}}
{{- if not .Values.redis.persistence.enabled -}}{{ fail "built-in Sentinel requires persistence" }}{{- end -}}
{{- if gt (len .Release.Name) 39 -}}{{ fail "built-in Sentinel release name must not exceed 39 characters" }}{{- end -}}
{{- if not (semverCompare ">=1.33.0-0" .Capabilities.KubeVersion.Version) -}}{{ fail "built-in Sentinel requires Kubernetes >=1.33" }}{{- end -}}
{{- if empty .Values.redis.sentinel.existingSecret -}}
{{- if not (regexMatch "^[A-Za-z0-9_-]{32,256}$" .Values.redis.password) -}}{{ fail "built-in Sentinel data password must be a 32-256 character safe token" }}{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9_-]{32,256}$" .Values.redis.sentinel.password) -}}{{ fail "built-in Sentinel password must be a 32-256 character safe token" }}{{- end -}}
{{- if eq .Values.redis.password .Values.redis.sentinel.password -}}{{ fail "built-in Sentinel requires separate data and Sentinel passwords" }}{{- end -}}
{{- end -}}
{{- if eq .Values.redis.sentinel.dataPasswordKey .Values.redis.sentinel.sentinelPasswordKey -}}{{ fail "built-in Sentinel authentication Secret keys must differ" }}{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.productionSafetyChecks -}}
  {{- if and .Values.redis.enabled (ne .Values.redis.mode "sentinel") -}}{{- fail "productionSafetyChecks requires external HA Redis or built-in Sentinel" -}}{{- end -}}
  {{- if and (not .Values.redis.enabled) (not .Values.redis.external.requireHA) -}}{{- fail "productionSafetyChecks requires redis.external.requireHA" -}}{{- end -}}
  {{- if .Values.config.workspace.allowMissingLSMForKind -}}{{- fail "productionSafetyChecks forbids allowMissingLSMForKind" -}}{{- end -}}
  {{- $lsm := lower (trim (include "sandbox.effectiveLSMProfile" .)) -}}
  {{- if or (empty $lsm) (eq $lsm "unconfined") (eq $lsm "label=disable") -}}
    {{- fail "productionSafetyChecks requires a confined config.workspace.lsmProfile" -}}
  {{- end -}}
{{- end -}}

{{- if not .Values.redis.enabled -}}
{{- $mode := .Values.redis.external.mode | default "standalone" -}}
{{- if and (ne $mode "sentinel") (not (empty .Values.redis.external.masterName)) -}}{{ fail "redis.external.masterName is only valid in sentinel mode" }}{{- end -}}
{{- if and (eq $mode "cluster") (ne (int .Values.redis.external.db) 0) -}}{{ fail "redis.external.db must be 0 in cluster mode" }}{{- end -}}
{{- if and (eq $mode "sentinel") (empty .Values.redis.external.masterName) -}}{{ fail "redis.external.masterName is required in sentinel mode" }}{{- end -}}
{{- if .Values.redis.external.requireHA -}}
{{- if eq $mode "standalone" -}}{{ fail "redis.external.requireHA rejects standalone mode" }}{{- end -}}
{{- if eq (.Values.redis.external.durability | default "best_effort") "best_effort" -}}{{ fail "redis.external.requireHA rejects best_effort durability" }}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "sandbox.builtinSentinel" -}}
{{- if and .Values.redis.enabled (eq .Values.redis.mode "sentinel") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "sandbox.sentinelName" -}}{{ printf "%s-redis-sentinel" .Release.Name }}{{- end -}}
{{- define "sandbox.sentinelIdentitySecretName" -}}
{{- .Values.redis.sentinel.identitySecretName | default (printf "%s-identity" (include "sandbox.sentinelName" .)) -}}
{{- end -}}

{{- /* 只缓存到 Helm ROOT context，不使用用户可伪造的 Values。 */ -}}
{{- define "sandbox.sentinelState" -}}
{{- if not (hasKey . "_sandboxSentinelState") -}}
{{- $name := include "sandbox.sentinelName" . -}}
{{- $members := include "sandbox.sentinelMembers" . | fromJsonArray -}}
{{- $retained := lookup "v1" "ConfigMap" .Release.Namespace (printf "%s-state" $name) -}}
{{- $identity := lookup "v1" "Secret" .Release.Namespace (include "sandbox.sentinelIdentitySecretName" .) -}}
{{- $installedSTS := lookup "apps/v1" "StatefulSet" .Release.Namespace $name -}}
{{- $hasPVC := false -}}
{{- range $ordinal := until 3 -}}
{{- if lookup "v1" "PersistentVolumeClaim" $.Release.Namespace (printf "data-%s-%d" $name $ordinal) -}}
{{- $hasPVC = true -}}
{{- end -}}
{{- end -}}
{{- if and $hasPVC (not $retained) -}}
{{- fail "retained Sentinel PVCs are missing the state ConfigMap; restore the original state and identity Secret, do not generate a new state" -}}
{{- end -}}
{{- if and (or $retained $hasPVC) (not $identity) -}}
{{- fail "retained Sentinel state/PVCs are missing the identity Secret; restore the original identity Secret, do not regenerate retained identity" -}}
{{- end -}}
{{- $data := dict -}}
{{- $clusterID := "" -}}
{{- $freshClusterID := "" -}}
{{- if $retained -}}
{{- if or (empty $retained.data) (empty (index $retained.data "cluster.json")) -}}
{{- fail "retained Sentinel state is missing cluster.json; do not reset existing PVC identity" -}}
{{- end -}}
{{- $cluster := index $retained.data "cluster.json" | fromJson -}}
{{- if ne (toJson $cluster.members) (toJson $members) -}}
{{- fail "retained Sentinel membership differs; do not reuse PVC identity under new DNS names" -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9_-]{1,128}$" (default "" $cluster.clusterID)) -}}
{{- fail "retained Sentinel state has an invalid clusterID; restore the original state" -}}
{{- end -}}
{{- $data = $retained.data -}}
{{- $clusterID = $cluster.clusterID -}}
{{- else -}}
{{- $clusterID = randAlphaNum 32 -}}
{{- $data = dict "cluster.json" (dict "clusterID" $clusterID "members" $members "phase" "Pending" | toJson) -}}
{{- if and (empty .Values.redis.sentinel.identitySecretName) (not $identity) (not $installedSTS) -}}
{{- $freshClusterID = $clusterID -}}
{{- end -}}
{{- end -}}
{{- $claimAnnotations := dict "sandbox/redis-cluster-id" $clusterID -}}
{{- if $installedSTS -}}
{{- $claims := $installedSTS.spec.volumeClaimTemplates | default list -}}
{{- if or (ne (len $claims) 1) (ne (index $claims 0).metadata.name "data") -}}
{{- fail "retained Sentinel StatefulSet PVC template is not the fixed data claim; do not rewrite existing PVC metadata" -}}
{{- end -}}
{{- /* VCT 不可升级变更：旧模板无标记不补写，仅保留现有 annotations。 */ -}}
{{- $claimAnnotations = (index $claims 0).metadata.annotations | default dict -}}
{{- if and (hasKey $claimAnnotations "sandbox/redis-cluster-id") (ne (get $claimAnnotations "sandbox/redis-cluster-id") $clusterID) -}}
{{- fail "retained Sentinel StatefulSet PVC template clusterID differs from state; restore the original objects, do not rewrite identity" -}}
{{- end -}}
{{- end -}}
{{- $_ := set . "_sandboxSentinelState" (dict "data" $data "clusterID" $clusterID "freshClusterID" $freshClusterID "claimAnnotations" $claimAnnotations) -}}
{{- end -}}
{{- get . "_sandboxSentinelState" | toJson -}}
{{- end -}}

{{- define "sandbox.sentinelIdentityJobName" -}}
{{- $memberName := include "sandbox.sentinelName" . -}}
{{- $revision := printf "%d" (int .Release.Revision) -}}
{{- $full := printf "%s-identity-%s" $memberName $revision -}}
{{- if le (len $full) 63 -}}{{ $full }}
{{- else -}}
{{- printf "%s-redis-id-%s-%s" (.Release.Name | trunc 20 | trimSuffix "-") ($memberName | sha256sum | trunc 10) $revision -}}
{{- end -}}
{{- end -}}
{{- define "sandbox.apiStartupFailureThreshold" -}}
{{- $threshold := int .Values.startupProbe.failureThreshold -}}
{{- if eq (include "sandbox.builtinSentinel" .) "true" -}}
{{- $period := int .Values.startupProbe.periodSeconds -}}
{{- $budget := add (max 600 (int .Values.redis.sentinel.initializeTimeoutSeconds)) 180 -}}
{{- /* Extra one probe accounts for the first failure occurring immediately. */ -}}
{{- $threshold = max $threshold (add 1 (div (add $budget (sub $period 1)) $period)) -}}
{{- end -}}
{{- $threshold -}}
{{- end -}}
{{- define "sandbox.sentinelJobName" -}}
{{- $memberName := include "sandbox.sentinelName" . -}}
{{- $revision := printf "%d" (int .Release.Revision) -}}
{{- $full := printf "%s-initialize-%s" $memberName $revision -}}
{{- if le (len $full) 63 -}}{{ $full }}
{{- else -}}
{{- printf "%s-redis-init-%s-%s" (.Release.Name | trunc 20 | trimSuffix "-") ($memberName | sha256sum | trunc 10) $revision -}}
{{- end -}}
{{- end -}}
{{- define "sandbox.sentinelMembers" -}}
{{- $name := include "sandbox.sentinelName" . -}}
{{- $members := list -}}
{{- range $ordinal := until 3 -}}
{{- $member := printf "%s-%d.%s-headless.%s.svc.%s" $name $ordinal $name $.Release.Namespace $.Values.redis.sentinel.clusterDomain -}}
{{- if gt (len $member) 253 -}}{{ fail "built-in Sentinel member DNS must not exceed 253 characters" }}{{- end -}}
{{- $members = append $members $member -}}
{{- end -}}
{{- $members | toJson -}}
{{- end -}}

{{- define "sandbox.redisEnv" -}}
{{- $sentinel := eq (include "sandbox.builtinSentinel" .) "true" -}}
{{- $ext := .Values.redis.external -}}
{{- $authSecret := printf "%s-redis" .Release.Name -}}
{{- if $sentinel -}}{{- $authSecret = .Values.redis.sentinel.existingSecret | default $authSecret -}}{{- end -}}
- name: SANDBOX_STORAGE_STATE_REDIS_ADDR
  value: {{ ternary "" (ternary (printf "%s-redis:6379" .Release.Name) $ext.addr .Values.redis.enabled) $sentinel | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_MODE
  value: {{ ternary "sentinel" (ternary "standalone" $ext.mode .Values.redis.enabled) $sentinel | quote }}
{{- if $sentinel }}
{{- $addrs := list }}
{{- range $member := include "sandbox.sentinelMembers" . | fromJsonArray }}
{{- $addrs = append $addrs (printf "%s:26379" $member) }}
{{- end }}
- name: SANDBOX_STORAGE_STATE_REDIS_ADDRS
  value: {{ join "," $addrs | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_MASTER_NAME
  value: {{ .Values.redis.sentinel.masterName | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_BOOTSTRAP_STATE_DIRECTORY
  value: /bootstrap
- name: SANDBOX_STORAGE_STATE_REDIS_BOOTSTRAP_PUBLIC_KEYS_FILE
  value: /identity-public/public-keys.json
{{- else if not .Values.redis.enabled }}
{{- if $ext.addrs }}
- name: SANDBOX_STORAGE_STATE_REDIS_ADDRS
  value: {{ join "," $ext.addrs | quote }}
{{- end }}
{{- if $ext.masterName }}
- name: SANDBOX_STORAGE_STATE_REDIS_MASTER_NAME
  value: {{ $ext.masterName | quote }}
{{- end }}
{{- if $ext.username }}
- name: SANDBOX_STORAGE_STATE_REDIS_USERNAME
  value: {{ $ext.username | quote }}
{{- end }}
{{- if $ext.sentinelUsername }}
- name: SANDBOX_STORAGE_STATE_REDIS_SENTINEL_USERNAME
  value: {{ $ext.sentinelUsername | quote }}
{{- end }}
{{- end }}
{{- if or $sentinel (ternary .Values.redis.password $ext.password .Values.redis.enabled) }}
- name: SANDBOX_STORAGE_STATE_REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $authSecret | quote }}
      key: {{ ternary .Values.redis.sentinel.dataPasswordKey "password" $sentinel | quote }}
{{- end }}
{{- if or $sentinel (and (not .Values.redis.enabled) $ext.sentinelPassword) }}
- name: SANDBOX_STORAGE_STATE_REDIS_SENTINEL_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $authSecret | quote }}
      key: {{ ternary .Values.redis.sentinel.sentinelPasswordKey "sentinel-password" $sentinel | quote }}
{{- end }}
- name: SANDBOX_STORAGE_STATE_REDIS_DB
  value: {{ ternary 0 (int $ext.db) .Values.redis.enabled | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_DURABILITY
  value: {{ ternary "replica_ack" (ternary "best_effort" $ext.durability .Values.redis.enabled) $sentinel | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_REQUIRE_HA
  value: {{ ternary true (ternary false $ext.requireHA .Values.redis.enabled) $sentinel | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_ACK_REPLICAS
  value: {{ ternary 1 (int $ext.ackReplicas) .Values.redis.enabled | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_ACK_TIMEOUT_MS
  value: {{ ternary (int .Values.redis.sentinel.ackTimeoutMs) (ternary 100 (int $ext.ackTimeoutMs) .Values.redis.enabled) $sentinel | quote }}
{{- end -}}

{{- define "sandbox.sentinelProcessEnv" -}}
- name: POD_ORDINAL
  valueFrom:
    fieldRef:
      fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
{{ include "sandbox.sentinelAuthEnv" . }}
{{- end -}}

{{- define "sandbox.sentinelAuthEnv" -}}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.redis.sentinel.existingSecret | default (printf "%s-redis" .Release.Name) | quote }}
      key: {{ .Values.redis.sentinel.dataPasswordKey | quote }}
- name: REDIS_SENTINEL_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.redis.sentinel.existingSecret | default (printf "%s-redis" .Release.Name) | quote }}
      key: {{ .Values.redis.sentinel.sentinelPasswordKey | quote }}
{{- end -}}

{{- define "sandbox.sentinelIdentityEnv" -}}
- name: POD_ORDINAL
  valueFrom:
    fieldRef:
      fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.redis.sentinel.existingSecret | default (printf "%s-redis" .Release.Name) | quote }}
      key: {{ .Values.redis.sentinel.dataPasswordKey | quote }}
{{- end -}}

{{- define "sandbox.sentinelProcessSecurity" -}}
runAsUser: 999
runAsGroup: 999
runAsNonRoot: true
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
seccompProfile:
  type: RuntimeDefault
{{- end -}}
{{- define "sandbox.backend.fingerprint" -}}
{{- $filesystem := .Values.config.storage.filesystem -}}
{{- $contract := dict
  "preset" $filesystem.preset
  "provider" (include "sandbox.backend.provider" .)
  "profile" (include "sandbox.backend.profile" .)
  "storageIdentity" $filesystem.storageIdentity
  "endpoint" $filesystem.endpoint
  "useSSL" $filesystem.useSSL
  "region" $filesystem.region
  "bucket" $filesystem.bucket
  "subPath" $filesystem.subPath
  "credentialGeneration" $filesystem.credentialGeneration
  "mounterImage" .Values.config.workspace.fuseImages.mounter
  "dockerImage" .Values.config.workspace.fuseImages.docker
  "lsmProfile" (include "sandbox.effectiveLSMProfile" .)
  "nodeSelector" (include "sandbox.effectiveNodeSelector" . | fromJson)
-}}
{{- $contract | toJson | sha256sum -}}
{{- end -}}

{{- define "sandbox.apiImage" -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}

{{- define "sandbox.drainEnv" -}}
{{- $sandboxNs := .Values.config.runtime.kubernetes.namespace | default .Release.Namespace -}}
- name: SANDBOX_RUNTIME_TYPE
  value: {{ .Values.config.runtime.type | quote }}
- name: SANDBOX_RUNTIME_KUBERNETES_NAMESPACE
  value: {{ $sandboxNs | quote }}
{{- $nodeSelector := include "sandbox.effectiveNodeSelector" . }}
{{- if ne $nodeSelector "{}" }}
- name: SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR
  value: {{ $nodeSelector | quote }}
{{- end }}
- name: SANDBOX_RUNTIME_KUBERNETES_NETWORK_POLICY_PROVIDER
  value: {{ .Values.config.runtime.kubernetes.networkPolicyProvider | default "auto" | quote }}
- name: SANDBOX_IMAGES_SANDBOX
  value: {{ .Values.config.images.sandbox | quote }}
- name: SANDBOX_IMAGES_GATEWAY
  value: {{ .Values.config.images.gateway | quote }}
{{ include "sandbox.redisEnv" . }}
- name: SANDBOX_STORAGE_FILESYSTEM_PROVIDER
  value: {{ include "sandbox.backend.provider" . | quote }}
- name: SANDBOX_STORAGE_FILESYSTEM_BUCKET
  value: {{ .Values.config.storage.filesystem.bucket | quote }}
- name: SANDBOX_STORAGE_FILESYSTEM_REGION
  value: {{ .Values.config.storage.filesystem.region | quote }}
- name: SANDBOX_STORAGE_FILESYSTEM_ENDPOINT
  value: {{ .Values.config.storage.filesystem.endpoint | quote }}
- name: SANDBOX_STORAGE_FILESYSTEM_SUB_PATH
  value: {{ .Values.config.storage.filesystem.subPath | quote }}
- name: SANDBOX_STORAGE_FILESYSTEM_USE_SSL
  value: {{ .Values.config.storage.filesystem.useSSL | quote }}
- name: SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY
  value: {{ .Values.config.storage.filesystem.accessKey | quote }}
- name: SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY
  value: {{ .Values.config.storage.filesystem.secretKey | quote }}
- name: SANDBOX_WORKSPACE_DEFAULT_MOUNT_MODE
  value: {{ .Values.config.workspace.defaultMountMode | quote }}
- name: SANDBOX_WORKSPACE_ENABLED_MOUNT_MODES
  value: {{ join "," .Values.config.workspace.enabledMountModes | quote }}
- name: SANDBOX_WORKSPACE_ALLOW_MISSING_LSM_FOR_KIND
  value: {{ .Values.config.workspace.allowMissingLSMForKind | default false | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_PRESET
  value: {{ .Values.config.storage.filesystem.preset | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_DRIVER
  value: s3fs
- name: SANDBOX_WORKSPACE_BACKEND_PROFILE
  value: {{ include "sandbox.backend.profile" . | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_STORAGE_IDENTITY
  value: {{ .Values.config.storage.filesystem.storageIdentity | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_MOUNTER_IMAGE
  value: {{ .Values.config.workspace.fuseImages.mounter | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_DOCKER_IMAGE
  value: {{ .Values.config.workspace.fuseImages.docker | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_CREDENTIAL_GENERATION
  value: {{ .Values.config.storage.filesystem.credentialGeneration | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_LSM_PROFILE
  value: {{ include "sandbox.effectiveLSMProfile" . | quote }}
{{- if or .Values.config.security.apiKey .Values.config.security.apiKeySecretName }}
- name: SANDBOX_SECURITY_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.config.security.apiKeySecretName | default (printf "%s-api" .Release.Name) }}
      key: api-key
{{- end }}
{{- end -}}
