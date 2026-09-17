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

{{/* 与 Sentinel 一致，仅缺失/null 补默认值；不依赖部署者替换自己的 values.yaml。 */}}
{{- define "sandbox.apparmorLoaderConfig" -}}
{{- $input := .Values.apparmorLoader -}}
{{- if kindIs "invalid" $input -}}{{- $input = dict -}}{{- end -}}
{{- if not (kindIs "map" $input) -}}{{ fail "apparmorLoader 必须是配置映射" }}{{- end -}}
{{- $config := dict "enabled" false "priorityClassName" ""
  "checkIntervalSeconds" 10 "parserTimeoutSeconds" 10 "startupTimeoutSeconds" 180
  "resources" (dict "requests" (dict "cpu" "25m" "memory" "32Mi") "limits" (dict "cpu" "250m" "memory" "128Mi")) -}}
{{- range $key, $value := $input -}}
{{- if and (ne $key "image") (not (kindIs "invalid" $value)) -}}{{- $_ := set $config $key $value -}}{{- end -}}
{{- end -}}
{{- $imageInput := get $input "image" -}}
{{- if or (not (hasKey $input "image")) (kindIs "invalid" $imageInput) -}}{{- $imageInput = dict -}}{{- end -}}
{{- if not (kindIs "map" $imageInput) -}}{{ fail "apparmorLoader.image 必须是配置映射" }}{{- end -}}
{{- $image := dict "repository" "registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader" "tag" "v0.1.0" "pullPolicy" "IfNotPresent" -}}
{{- range $key, $value := $imageInput -}}
{{- if not (kindIs "invalid" $value) -}}{{- $_ := set $image $key $value -}}{{- end -}}
{{- end -}}
{{- $_ := set $config "image" $image -}}
{{- if not (kindIs "bool" $config.enabled) -}}{{ fail "apparmorLoader.enabled 必须是布尔值" }}{{- end -}}
{{- if not (kindIs "string" $config.priorityClassName) -}}{{ fail "apparmorLoader.priorityClassName 必须是字符串" }}{{- end -}}
{{- if and (ne $config.priorityClassName "") (or (gt (len $config.priorityClassName) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$" $config.priorityClassName))) -}}
{{- fail "apparmorLoader.priorityClassName 必须为空或有效 DNS 名称" -}}
{{- end -}}
{{- range $key := list "repository" "tag" "pullPolicy" -}}
{{- if not (kindIs "string" (get $image $key)) -}}{{ fail (printf "apparmorLoader.image.%s 必须是字符串" $key) }}{{- end -}}
{{- if empty (get $image $key) -}}{{ fail (printf "apparmorLoader.image.%s 不得为空" $key) }}{{- end -}}
{{- end -}}
{{- if not (has $image.pullPolicy (list "Always" "IfNotPresent" "Never")) -}}{{ fail "apparmorLoader.image.pullPolicy 必须是 Always、IfNotPresent 或 Never" }}{{- end -}}
{{- range $key := list "checkIntervalSeconds" "parserTimeoutSeconds" "startupTimeoutSeconds" -}}
{{- $value := get $config $key -}}
{{- if or (kindIs "string" $value) (not (regexMatch "^[0-9]+$" (printf "%v" $value))) (lt (int64 $value) 1) (gt (int64 $value) 600) -}}
{{- fail (printf "apparmorLoader.%s 必须是 1~600 范围内的整数" $key) -}}
{{- end -}}
{{- end -}}
{{- if not (kindIs "map" $config.resources) -}}{{ fail "apparmorLoader.resources 必须是资源配置映射" }}{{- end -}}
{{- $config | toJson -}}
{{- end -}}

{{- define "sandbox.apparmor.digest" -}}
{{- $template := .Files.Get "files/apparmor/workspace-mounter.profile" | replace "\r\n" "\n" | trim -}}
{{- if empty $template -}}{{ fail "AppArmor profile template is missing" }}{{- end -}}
{{- printf "%s\n" $template | sha256sum -}}
{{- end -}}

{{- define "sandbox.effectiveLSMProfile" -}}
{{- $apparmorConfig := include "sandbox.apparmorLoaderConfig" . | fromJson -}}
{{- if $apparmorConfig.enabled -}}
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
{{- $apparmorConfig := include "sandbox.apparmorLoaderConfig" . | fromJson -}}
{{- if $apparmorConfig.enabled -}}
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
{{- $apparmorConfig := include "sandbox.apparmorLoaderConfig" . | fromJson -}}
{{- if $apparmorConfig.enabled -}}
{{- if ne .Values.config.runtime.type "kubernetes" -}}{{ fail "AppArmor loader requires Kubernetes runtime" }}{{- end -}}
{{- if ne (include "sandbox.fuseEnabled" .) "true" -}}{{ fail "AppArmor loader requires FUSE" }}{{- end -}}
{{- if .Values.config.workspace.allowMissingLSMForKind -}}{{ fail "AppArmor loader forbids allowMissingLSMForKind" }}{{- end -}}
{{- end -}}
{{- end -}}

{{/* 默认值在模板中统一解析，不依赖部署者完整复制新版 values.yaml/schema。
     仅缺失或 null 使用默认值；显式的 false、0、空名称等非法值不能被 default 掩盖。 */}}
{{- define "sandbox.sentinelConfig" -}}
{{- $input := .Values.redis.sentinel -}}
{{- if kindIs "invalid" $input -}}{{- $input = dict -}}{{- end -}}
{{- if not (kindIs "map" $input) -}}{{ fail "redis.sentinel 必须是配置映射" }}{{- end -}}
{{- $config := dict
  "masterName" "sandbox" "clusterDomain" "cluster.local"
  "identitySecretName" "" "existingSecret" ""
  "dataPasswordKey" "password" "sentinelPasswordKey" "sentinel-password" "password" ""
  "downAfterMilliseconds" 10000 "failoverTimeoutMilliseconds" 60000 "parallelSyncs" 1
  "startupTimeoutSeconds" 600 "initializeTimeoutSeconds" 720 "ackTimeoutMs" 1000
  "resources" (dict "requests" (dict "cpu" "50m" "memory" "64Mi") "limits" (dict "cpu" "250m" "memory" "128Mi"))
  "identityResources" (dict "requests" (dict "cpu" "10m" "memory" "64Mi") "limits" (dict "cpu" "200m" "memory" "256Mi")) -}}
{{- range $key, $value := $input -}}
{{- if and (ne $key "bootstrapImage") (not (kindIs "invalid" $value)) -}}{{- $_ := set $config $key $value -}}{{- end -}}
{{- end -}}
{{- $dnsPattern := "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$" -}}
{{- range $key := list "masterName" "clusterDomain" "identitySecretName" "existingSecret" "dataPasswordKey" "sentinelPasswordKey" "password" -}}
{{- if not (kindIs "string" (get $config $key)) -}}{{ fail (printf "redis.sentinel.%s 必须是字符串" $key) }}{{- end -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9_-]{1,64}$" $config.masterName) -}}{{ fail "redis.sentinel.masterName 必须是 1~64 位字母、数字、_ 或 -" }}{{- end -}}
{{- if or (gt (len $config.clusterDomain) 128) (not (regexMatch $dnsPattern $config.clusterDomain)) -}}{{ fail "redis.sentinel.clusterDomain 必须是有效 DNS 后缀，且不超过 128 字符" }}{{- end -}}
{{- range $key := list "identitySecretName" "existingSecret" -}}
{{- $value := get $config $key -}}
{{- if and (ne $value "") (or (gt (len $value) 253) (not (regexMatch $dnsPattern $value))) -}}{{ fail (printf "redis.sentinel.%s 必须为空或有效 Secret 名称" $key) }}{{- end -}}
{{- end -}}
{{- range $key := list "dataPasswordKey" "sentinelPasswordKey" -}}
{{- $value := get $config $key -}}
{{- if or (gt (len $value) 253) (not (regexMatch "^[A-Za-z0-9_.-]+$" $value)) -}}{{ fail (printf "redis.sentinel.%s 必须是有效且非空的 Secret 键名" $key) }}{{- end -}}
{{- end -}}
{{- $bounds := dict "startupTimeoutSeconds" (list 1 600) "initializeTimeoutSeconds" (list 1 720)
  "ackTimeoutMs" (list 1000 10000) "downAfterMilliseconds" (list 1000 60000)
  "failoverTimeoutMilliseconds" (list 2000 600000) "parallelSyncs" (list 1 2) -}}
{{- range $key, $range := $bounds -}}
{{- $value := get $config $key -}}
{{- if or (kindIs "string" $value) (not (regexMatch "^[0-9]+$" (printf "%v" $value))) (lt (int64 $value) (int64 (index $range 0))) (gt (int64 $value) (int64 (index $range 1))) -}}
{{- fail (printf "redis.sentinel.%s 必须是 %d~%d 范围内的整数" $key (index $range 0) (index $range 1)) -}}
{{- end -}}
{{- end -}}
{{- if lt (int64 $config.failoverTimeoutMilliseconds) (mul 2 (int64 $config.downAfterMilliseconds)) -}}{{ fail "redis.sentinel.failoverTimeoutMilliseconds 必须至少为 downAfterMilliseconds 的两倍" }}{{- end -}}
{{- range $key := list "resources" "identityResources" -}}
{{- if not (kindIs "map" (get $config $key)) -}}{{ fail (printf "redis.sentinel.%s 必须是资源配置映射" $key) }}{{- end -}}
{{- end -}}
{{- $config | toJson -}}
{{- end -}}

{{- define "sandbox.sentinelBootstrapImageConfig" -}}
{{- $sentinel := .Values.redis.sentinel -}}
{{- if not (kindIs "map" $sentinel) -}}{{ fail "redis.sentinel 必须是配置映射" }}{{- end -}}
{{- $input := get $sentinel "bootstrapImage" -}}
{{- if or (kindIs "invalid" $input) (not (kindIs "map" $input)) -}}{{ fail "redis.sentinel.bootstrapImage 必须是配置映射" }}{{- end -}}
{{- $image := dict -}}
{{- range $key := list "repository" "tag" "pullPolicy" -}}
{{- $value := get $input $key -}}
{{- if not (kindIs "string" $value) -}}{{ fail (printf "redis.sentinel.bootstrapImage.%s 必须是字符串" $key) }}{{- end -}}
{{- if empty $value -}}{{ fail (printf "redis.sentinel.bootstrapImage.%s 不得为空" $key) }}{{- end -}}
{{- $_ := set $image $key $value -}}
{{- end -}}
{{- if not (has $image.pullPolicy (list "Always" "IfNotPresent" "Never")) -}}{{ fail "redis.sentinel.bootstrapImage.pullPolicy 必须是 Always、IfNotPresent 或 Never" }}{{- end -}}
{{- $image | toJson -}}
{{- end -}}

{{- define "sandbox.sentinelBootstrapImage" -}}
{{- $image := include "sandbox.sentinelBootstrapImageConfig" . | fromJson -}}
{{- printf "%s:%s" $image.repository $image.tag -}}
{{- end -}}

{{- define "sandbox.apiStartupProbe" -}}
{{- $input := .Values.startupProbe -}}
{{- if kindIs "invalid" $input -}}{{- $input = dict -}}{{- end -}}
{{- if not (kindIs "map" $input) -}}{{ fail "startupProbe 必须是配置映射" }}{{- end -}}
{{- $probe := dict "enabled" true "periodSeconds" 5 "timeoutSeconds" 1 "failureThreshold" 120 -}}
{{- range $key, $value := $input -}}
{{- if not (kindIs "invalid" $value) -}}{{- $_ := set $probe $key $value -}}{{- end -}}
{{- end -}}
{{- if eq (include "sandbox.builtinSentinel" .) "true" -}}
{{- if or (not (kindIs "bool" $probe.enabled)) (not $probe.enabled) -}}{{ fail "built-in Sentinel requires the API startup probe (startupProbe.enabled 必须为 true)" }}{{- end -}}
{{- range $key := list "periodSeconds" "timeoutSeconds" "failureThreshold" -}}
{{- $value := get $probe $key -}}
{{- $maximum := ternary 2147483647 60 (eq $key "failureThreshold") -}}
{{- if or (kindIs "string" $value) (not (regexMatch "^[0-9]+$" (printf "%v" $value))) (lt (int64 $value) 1) (gt (int64 $value) (int64 $maximum)) -}}{{ fail (printf "startupProbe.%s 必须是 1~%d 范围内的整数" $key $maximum) }}{{- end -}}
{{- end -}}
{{- end -}}
{{- $probe | toJson -}}
{{- end -}}

{{- define "sandbox.validateRedis" -}}
{{- if .Values.redis.enabled -}}
{{- if not (has .Values.redis.mode (list "standalone" "sentinel")) -}}{{ fail "redis.mode must be standalone or sentinel" }}{{- end -}}
{{- if eq .Values.redis.mode "sentinel" -}}
{{- $sentinelConfig := include "sandbox.sentinelConfig" . | fromJson -}}
{{- $_ := include "sandbox.sentinelBootstrapImageConfig" . -}}
{{- $_ := include "sandbox.apiStartupProbe" . -}}
{{- if not .Values.redis.persistence.enabled -}}{{ fail "built-in Sentinel requires persistence" }}{{- end -}}
{{- if gt (len .Release.Name) 39 -}}{{ fail "built-in Sentinel release name must not exceed 39 characters" }}{{- end -}}
{{- if not (semverCompare ">=1.29.0-0" .Capabilities.KubeVersion.Version) -}}{{ fail "built-in Sentinel requires Kubernetes >=1.29 with SidecarContainers and PodIndexLabel enabled" }}{{- end -}}
{{- if empty $sentinelConfig.existingSecret -}}
{{- if not (regexMatch "^[A-Za-z0-9_-]{32,256}$" .Values.redis.password) -}}{{ fail "built-in Sentinel data password must be a 32-256 character safe token" }}{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9_-]{32,256}$" $sentinelConfig.password) -}}{{ fail "built-in Sentinel password must be a 32-256 character safe token" }}{{- end -}}
{{- if eq .Values.redis.password $sentinelConfig.password -}}{{ fail "built-in Sentinel requires separate data and Sentinel passwords" }}{{- end -}}
{{- end -}}
{{- if eq $sentinelConfig.dataPasswordKey $sentinelConfig.sentinelPasswordKey -}}{{ fail "built-in Sentinel authentication Secret keys must differ" }}{{- end -}}
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
{{- $sentinelConfig := include "sandbox.sentinelConfig" . | fromJson -}}
{{- $sentinelConfig.identitySecretName | default (printf "%s-identity" (include "sandbox.sentinelName" .)) -}}
{{- end -}}

{{- /* 只缓存到 Helm ROOT context，不使用用户可伪造的 Values。 */ -}}
{{- define "sandbox.sentinelState" -}}
{{- if not (hasKey . "_sandboxSentinelState") -}}
{{- $name := include "sandbox.sentinelName" . -}}
{{- $sentinelConfig := include "sandbox.sentinelConfig" . | fromJson -}}
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
{{- $phase := "Pending" -}}
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
{{- $phase = default "" $cluster.phase -}}
{{- if not (has $phase (list "Pending" "Initialized")) -}}
{{- fail "retained Sentinel state has an invalid phase; restore the original state" -}}
{{- end -}}
{{- $data = $retained.data -}}
{{- $clusterID = $cluster.clusterID -}}
{{- else -}}
{{- $clusterID = randAlphaNum 32 -}}
{{- $data = dict "cluster.json" (dict "clusterID" $clusterID "members" $members "phase" "Pending" | toJson) -}}
{{- if and (empty $sentinelConfig.identitySecretName) (not $identity) (not $installedSTS) -}}
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
{{- $_ := set . "_sandboxSentinelState" (dict "data" $data "clusterID" $clusterID "freshClusterID" $freshClusterID "claimAnnotations" $claimAnnotations "identityExists" (not (empty $identity)) "phase" $phase) -}}
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
{{- $probe := include "sandbox.apiStartupProbe" . | fromJson -}}
{{- $threshold := int $probe.failureThreshold -}}
{{- if eq (include "sandbox.builtinSentinel" .) "true" -}}
{{- $sentinelConfig := include "sandbox.sentinelConfig" . | fromJson -}}
{{- $period := int $probe.periodSeconds -}}
{{- $budget := add (max 600 (int $sentinelConfig.initializeTimeoutSeconds)) 225 -}}
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
{{- $sentinelConfig := include "sandbox.sentinelConfig" . | fromJson -}}
{{- $members := list -}}
{{- range $ordinal := until 3 -}}
{{- $member := printf "%s-%d.%s-headless.%s.svc.%s" $name $ordinal $name $.Release.Namespace $sentinelConfig.clusterDomain -}}
{{- if gt (len $member) 253 -}}{{ fail "built-in Sentinel member DNS must not exceed 253 characters" }}{{- end -}}
{{- $members = append $members $member -}}
{{- end -}}
{{- $members | toJson -}}
{{- end -}}

{{- define "sandbox.redisEnv" -}}
{{- $sentinel := eq (include "sandbox.builtinSentinel" .) "true" -}}
{{- $sentinelConfig := dict -}}
{{- if $sentinel -}}{{- $sentinelConfig = include "sandbox.sentinelConfig" . | fromJson -}}{{- end -}}
{{- $ext := .Values.redis.external -}}
{{- $authSecret := printf "%s-redis" .Release.Name -}}
{{- if $sentinel -}}{{- $authSecret = $sentinelConfig.existingSecret | default $authSecret -}}{{- end -}}
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
  value: {{ $sentinelConfig.masterName | quote }}
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
      key: {{ ternary $sentinelConfig.dataPasswordKey "password" $sentinel | quote }}
{{- end }}
{{- if or $sentinel (and (not .Values.redis.enabled) $ext.sentinelPassword) }}
- name: SANDBOX_STORAGE_STATE_REDIS_SENTINEL_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $authSecret | quote }}
      key: {{ ternary $sentinelConfig.sentinelPasswordKey "sentinel-password" $sentinel | quote }}
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
  value: {{ ternary (int $sentinelConfig.ackTimeoutMs) (ternary 100 (int $ext.ackTimeoutMs) .Values.redis.enabled) $sentinel | quote }}
{{- end -}}

{{- define "sandbox.sentinelProcessEnv" -}}
- name: POD_ORDINAL
  valueFrom:
    fieldRef:
      fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
{{ include "sandbox.sentinelAuthEnv" . }}
{{- end -}}

{{- define "sandbox.sentinelAuthEnv" -}}
{{- $sentinelConfig := include "sandbox.sentinelConfig" . | fromJson -}}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $sentinelConfig.existingSecret | default (printf "%s-redis" .Release.Name) | quote }}
      key: {{ $sentinelConfig.dataPasswordKey | quote }}
- name: REDIS_SENTINEL_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $sentinelConfig.existingSecret | default (printf "%s-redis" .Release.Name) | quote }}
      key: {{ $sentinelConfig.sentinelPasswordKey | quote }}
{{- end -}}

{{- define "sandbox.sentinelIdentityEnv" -}}
{{- $sentinelConfig := include "sandbox.sentinelConfig" . | fromJson -}}
- name: POD_ORDINAL
  valueFrom:
    fieldRef:
      fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $sentinelConfig.existingSecret | default (printf "%s-redis" .Release.Name) | quote }}
      key: {{ $sentinelConfig.dataPasswordKey | quote }}
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
