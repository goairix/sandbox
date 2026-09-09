{{- define "sandbox.backend.provider" -}}
{{- if eq .Values.config.storage.filesystem.preset "minio" -}}minio
{{- else if or (eq .Values.config.storage.filesystem.preset "huawei-obs-public") (eq .Values.config.storage.filesystem.preset "huawei-obs-private") -}}obs
{{- else -}}{{ fail (printf "unsupported config.storage.filesystem.preset %q" .Values.config.storage.filesystem.preset) }}
{{- end -}}
{{- end -}}

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

{{- define "sandbox.backend.endpointHost" -}}
{{- $withoutScheme := regexReplaceAll "^https?://" .Values.config.storage.filesystem.endpoint "" -}}
{{- $withoutPath := first (splitList "/" $withoutScheme) -}}
{{- regexReplaceAll ":[0-9]+$" $withoutPath "" -}}
{{- end -}}

{{- define "sandbox.backend.systemEgressFQDNs" -}}
{{- $host := include "sandbox.backend.endpointHost" . -}}
{{- if eq (include "sandbox.backend.provider" .) "obs" -}}
{{- printf "%s,%s.%s" $host .Values.config.storage.filesystem.bucket $host -}}
{{- else -}}
{{- $host -}}
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
  "caSecretName" .Values.workspaceCA.secretName
  "credentialGeneration" $filesystem.credentialGeneration
  "caSecretKey" $filesystem.caSecretKey
  "endpointHostIPs" $filesystem.endpointHostIPs
  "systemEgressMode" $filesystem.systemEgressMode
  "systemEgressFQDNs" (include "sandbox.backend.systemEgressFQDNs" .)
  "dnsCIDRs" $filesystem.dnsCIDRs
  "systemEgressCIDRs" $filesystem.systemEgressCIDRs
  "endpointPorts" $filesystem.endpointPorts
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
{{- if $redisPassword }}
- name: SANDBOX_STORAGE_STATE_REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ printf "%s-redis" .Release.Name }}
      key: password
{{- end }}
- name: SANDBOX_STORAGE_STATE_REDIS_DB
  value: {{ ternary 0 (.Values.redis.external.db | int) .Values.redis.enabled | quote }}
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
- name: SANDBOX_STORAGE_FILESYSTEM_CA_FILE
  value: {{ ternary "/run/secrets/workspace/ca.crt" "" (ne (.Values.config.storage.filesystem.caSecretKey | default "") "") | quote }}
- name: SANDBOX_WORKSPACE_DEFAULT_MOUNT_MODE
  value: {{ .Values.config.workspace.defaultMountMode | quote }}
- name: SANDBOX_WORKSPACE_ENABLED_MOUNT_MODES
  value: {{ join "," .Values.config.workspace.enabledMountModes | quote }}
- name: SANDBOX_WORKSPACE_SECRET_NAME
  value: {{ .Values.workspaceCA.secretName | default "" | quote }}
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
- name: SANDBOX_WORKSPACE_BACKEND_CA_SECRET_KEY
  value: {{ .Values.config.storage.filesystem.caSecretKey | default "" | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_CREDENTIAL_GENERATION
  value: {{ .Values.config.storage.filesystem.credentialGeneration | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_ENDPOINT_HOST_IPS
  value: {{ join "," (.Values.config.storage.filesystem.endpointHostIPs | default list) | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_LSM_PROFILE
  value: {{ .Values.config.workspace.lsmProfile | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_SYSTEM_EGRESS_MODE
  value: {{ .Values.config.storage.filesystem.systemEgressMode | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_DNS_CIDRS
  value: {{ join "," .Values.config.storage.filesystem.dnsCIDRs | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_SYSTEM_EGRESS_FQDNS
  value: {{ include "sandbox.backend.systemEgressFQDNs" . | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_SYSTEM_EGRESS_CIDRS
  value: {{ join "," (.Values.config.storage.filesystem.systemEgressCIDRs | default list) | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_ENDPOINT_PORTS
  value: {{ join "," .Values.config.storage.filesystem.endpointPorts | quote }}
- name: SANDBOX_WORKSPACE_BACKEND_PROXY_URL
  value: {{ .Values.config.workspace.proxyURL | default "" | quote }}
{{- if or .Values.config.security.apiKey .Values.config.security.apiKeySecretName }}
- name: SANDBOX_SECURITY_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.config.security.apiKeySecretName | default (printf "%s-api" .Release.Name) }}
      key: api-key
{{- end }}
{{- end -}}
