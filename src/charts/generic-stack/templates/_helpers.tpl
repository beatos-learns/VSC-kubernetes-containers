{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.name" -}}
{{- default .Release.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.componentName" -}}
{{- printf "%s-%s" (include "generic-stack.name" .root) .name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- with .spec.image.tag }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
app.kubernetes.io/part-of: {{ include "generic-stack.name" .root }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.selectorLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.image" -}}
{{- $registry := .spec.image.registry | default .root.Values.global.imageRegistry -}}
{{- $repository := required (printf "components.%s.image.repository is required" .name) .spec.image.repository -}}
{{- if .spec.image.digest -}}
{{- printf "%s/%s@%s" $registry $repository .spec.image.digest -}}
{{- else -}}
{{- printf "%s/%s:%s" $registry $repository (toString (required (printf "components.%s.image.tag or image.digest is required" .name) .spec.image.tag)) -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.secretName" -}}
{{- if .spec.existingSecret -}}
{{- tpl .spec.existingSecret .root -}}
{{- else -}}
{{- include "generic-stack.componentName" (dict "root" .root "name" .name) -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.configChecksum" -}}
{{- $parts := list (tpl (toYaml .spec.files) .root) -}}
{{- if not .spec.existingSecret -}}
{{- $parts = append $parts (tpl (toYaml .spec.secret) .root) -}}
{{- end -}}
{{- join "|" $parts | sha256sum -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.probe" -}}
httpGet:
  path: {{ .path }}
  port: {{ .port }}
periodSeconds: {{ .cfg.periodSeconds }}
failureThreshold: {{ .cfg.failureThreshold }}
timeoutSeconds: {{ .cfg.timeoutSeconds }}
{{- with .cfg.initialDelaySeconds }}
initialDelaySeconds: {{ . }}
{{- end }}
{{- end -}}
