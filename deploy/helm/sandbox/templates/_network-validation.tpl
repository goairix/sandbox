{{- define "sandbox.validateNetworkCIDRs" -}}
{{- $pods := .Values.config.runtime.kubernetes.podCIDRs | default (list) -}}
{{- $services := .Values.config.runtime.kubernetes.serviceCIDRs | default (list) -}}
{{- if ne (empty $pods) (empty $services) -}}
{{- fail "config.runtime.kubernetes podCIDRs and serviceCIDRs must both be configured or both empty" -}}
{{- end -}}
{{- if or (gt (len $pods) 256) (gt (len $services) 256) -}}
{{- fail "authoritative Kubernetes CIDR sets exceed 256 entries" -}}
{{- end -}}
{{- $podFamilies := dict -}}
{{- $serviceFamilies := dict -}}
{{- range $pods -}}
{{- $_ := set $podFamilies (ternary "6" "4" (contains ":" (toString .))) true -}}
{{- end -}}
{{- range $services -}}
{{- $_ := set $serviceFamilies (ternary "6" "4" (contains ":" (toString .))) true -}}
{{- end -}}
{{- if or (ne (hasKey $podFamilies "4") (hasKey $serviceFamilies "4")) (ne (hasKey $podFamilies "6") (hasKey $serviceFamilies "6")) -}}
{{- fail "authoritative Kubernetes podCIDRs and serviceCIDRs must cover matching address families" -}}
{{- end -}}
{{- end -}}
