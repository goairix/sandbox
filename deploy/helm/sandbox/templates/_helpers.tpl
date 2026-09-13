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

{{- define "sandbox.validateRedis" -}}
{{- if .Values.productionSafetyChecks -}}
  {{- if .Values.redis.enabled -}}{{- fail "productionSafetyChecks requires external HA Redis" -}}{{- end -}}
  {{- if not .Values.redis.external.requireHA -}}{{- fail "productionSafetyChecks requires redis.external.requireHA" -}}{{- end -}}
  {{- if .Values.config.workspace.allowMissingLSMForKind -}}{{- fail "productionSafetyChecks forbids allowMissingLSMForKind" -}}{{- end -}}
  {{- $lsm := lower (trim (.Values.config.workspace.lsmProfile | default "")) -}}
  {{- if or (empty $lsm) (eq $lsm "unconfined") (eq $lsm "label=disable") -}}
    {{- fail "productionSafetyChecks requires a confined config.workspace.lsmProfile" -}}
  {{- end -}}
{{- end -}}
{{- if not .Values.redis.enabled -}}
  {{- $mode := .Values.redis.external.mode | default "standalone" -}}
  {{- if and (eq $mode "cluster") (ne (int .Values.redis.external.db) 0) -}}
    {{- fail "redis.external.db must be 0 in cluster mode" -}}
  {{- end -}}
  {{- if and (eq $mode "sentinel") (empty .Values.redis.external.masterName) -}}
    {{- fail "redis.external.masterName is required in sentinel mode" -}}
  {{- end -}}
  {{- if .Values.redis.external.requireHA -}}
    {{- if eq $mode "standalone" -}}{{- fail "redis.external.requireHA rejects standalone mode" -}}{{- end -}}
    {{- if eq (.Values.redis.external.durability | default "best_effort") "best_effort" -}}{{- fail "redis.external.requireHA rejects best_effort durability" -}}{{- end -}}
  {{- end -}}
{{- end -}}
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
-}}
{{- $contract | toJson | sha256sum -}}
{{- end -}}

{{- define "sandbox.apiImage" -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}

{{- define "sandbox.drainEnv" -}}
{{- $sandboxNs := .Values.config.runtime.kubernetes.namespace | default .Release.Namespace -}}
{{- $redisAddr := ternary (printf "%s-redis:6379" .Release.Name) .Values.redis.external.addr .Values.redis.enabled -}}
{{- $redisPassword := ternary .Values.redis.password .Values.redis.external.password .Values.redis.enabled -}}
{{- $redisMode := ternary "standalone" .Values.redis.external.mode .Values.redis.enabled -}}
{{- $redisDurability := ternary "best_effort" .Values.redis.external.durability .Values.redis.enabled -}}
- name: SANDBOX_RUNTIME_TYPE
  value: {{ .Values.config.runtime.type | quote }}
- name: SANDBOX_RUNTIME_KUBERNETES_NAMESPACE
  value: {{ $sandboxNs | quote }}
- name: SANDBOX_IMAGES_SANDBOX
  value: {{ .Values.config.images.sandbox | quote }}
- name: SANDBOX_IMAGES_GATEWAY
  value: {{ .Values.config.images.gateway | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_ADDR
  value: {{ $redisAddr | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_MODE
  value: {{ $redisMode | quote }}
{{- if and (not .Values.redis.enabled) .Values.redis.external.addrs }}
- name: SANDBOX_STORAGE_STATE_REDIS_ADDRS
  value: {{ join "," .Values.redis.external.addrs | quote }}
{{- end }}
{{- if and (not .Values.redis.enabled) .Values.redis.external.masterName }}
- name: SANDBOX_STORAGE_STATE_REDIS_MASTER_NAME
  value: {{ .Values.redis.external.masterName | quote }}
{{- end }}
{{- if and (not .Values.redis.enabled) .Values.redis.external.username }}
- name: SANDBOX_STORAGE_STATE_REDIS_USERNAME
  value: {{ .Values.redis.external.username | quote }}
{{- end }}
{{- if $redisPassword }}
- name: SANDBOX_STORAGE_STATE_REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ printf "%s-redis" .Release.Name }}
      key: password
{{- end }}
- name: SANDBOX_STORAGE_STATE_REDIS_DB
  value: {{ ternary 0 (.Values.redis.external.db | int) .Values.redis.enabled | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_DURABILITY
  value: {{ $redisDurability | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_REQUIRE_HA
  value: {{ ternary false .Values.redis.external.requireHA .Values.redis.enabled | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_ACK_REPLICAS
  value: {{ ternary 1 (.Values.redis.external.ackReplicas | int) .Values.redis.enabled | quote }}
- name: SANDBOX_STORAGE_STATE_REDIS_ACK_TIMEOUT_MS
  value: {{ ternary 100 (.Values.redis.external.ackTimeoutMs | int) .Values.redis.enabled | quote }}
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
  value: {{ .Values.config.workspace.lsmProfile | quote }}
{{- if or .Values.config.security.apiKey .Values.config.security.apiKeySecretName }}
- name: SANDBOX_SECURITY_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.config.security.apiKeySecretName | default (printf "%s-api" .Release.Name) }}
      key: api-key
{{- end }}
{{- end -}}
