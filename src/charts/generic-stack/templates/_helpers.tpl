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
{{- $existing := .spec.existingSecret | default .root.Values.global.existingSecret -}}
{{- if $existing -}}
{{- tpl $existing .root -}}
{{- else -}}
{{- include "generic-stack.componentName" (dict "root" .root "name" .name) -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{/* The chart-wide `monitoring` block mapped onto a component's env surface:
     the ports the pod declares, the logging knobs through the component's
     `monitoring.*Env` names, the health-check and shutdown knobs under their
     image-wide names; null keeps the image default */}}
{{- define "generic-stack.monitoringEnv" -}}
{{- $env := dict -}}
{{- $mon := .root.Values.monitoring -}}
{{- $map := .spec.monitoring -}}
{{- $_ := set $env $map.portEnv (printf $map.portFormat (int .spec.ports.main.containerPort)) -}}
{{- $_ := set $env $map.adminPortEnv (toString (int .spec.ports.admin.containerPort)) -}}
{{- range $port := .spec.ports.extra -}}
{{- if $port.env -}}{{- $_ := set $env $port.env (printf $map.portFormat (int $port.containerPort)) -}}{{- end -}}
{{- end -}}
{{- if not (kindIs "invalid" $mon.logging.level) -}}
{{- $level := toString $mon.logging.level -}}
{{- if $map.upperCaseLevel -}}{{- $level = upper $level -}}{{- end -}}
{{- $_ := set $env $map.levelEnv $level -}}
{{- end -}}
{{- if not (kindIs "invalid" $mon.logging.format) -}}
{{- $format := toString $mon.logging.format -}}
{{- $_ := set $env $map.formatEnv (index $map.formatValues $format | default $format) -}}
{{- end -}}
{{- if not (kindIs "invalid" $mon.logging.access) -}}
{{- $_ := set $env $map.accessEnv (toString $mon.logging.access) -}}
{{- end -}}
{{- range $key, $name := dict "interval" "HEALTH_CHECK_INTERVAL" "timeout" "HEALTH_CHECK_TIMEOUT" "staleFactor" "HEALTH_STALE_FACTOR" -}}
{{- $value := index $mon.healthCheck $key -}}
{{- if not (kindIs "invalid" $value) -}}{{- $_ := set $env $name (toString $value) -}}{{- end -}}
{{- end -}}
{{- range $key, $name := dict "drainDelay" "SHUTDOWN_DRAIN_DELAY" "timeout" "SHUTDOWN_TIMEOUT" -}}
{{- $value := index $mon.shutdown $key -}}
{{- if not (kindIs "invalid" $value) -}}{{- $_ := set $env $name (toString $value) -}}{{- end -}}
{{- end -}}
{{- toYaml $env -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{/* Effective health-check and shutdown values of a component: the chart-wide
     `monitoring` value when set, else the image default the component declares */}}
{{- define "generic-stack.effectiveMonitoring" -}}
{{- $mon := .root.Values.monitoring -}}
{{- $own := .spec.monitoring -}}
{{- $out := dict -}}
{{- range $key := list "interval" "timeout" "staleFactor" -}}
{{- $value := index $mon.healthCheck $key -}}
{{- if kindIs "invalid" $value -}}{{- $value = index $own.healthCheck $key -}}{{- end -}}
{{- $_ := set $out $key $value -}}
{{- end -}}
{{- range $key := list "drainDelay" "timeout" -}}
{{- $value := index $mon.shutdown $key -}}
{{- if kindIs "invalid" $value -}}{{- $value = index $own.shutdown $key -}}{{- end -}}
{{- $_ := set $out (printf "shutdown%s" (title $key)) $value -}}
{{- end -}}
{{- $_ := set $out "escalation" $own.shutdown.escalation -}}
{{- toYaml $out -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{/* terminationGracePeriodSeconds of a component: the explicit value when set
     (it must cover the drain), else drain delay + budget + escalation + margin */}}
{{- define "generic-stack.gracePeriod" -}}
{{- $eff := fromYaml (include "generic-stack.effectiveMonitoring" .) -}}
{{- $needed := add (int $eff.shutdownDrainDelay) (int $eff.shutdownTimeout) (int $eff.escalation) -}}
{{- if kindIs "invalid" .spec.terminationGracePeriodSeconds -}}
{{- add $needed (int .root.Values.monitoring.shutdown.margin) -}}
{{- else if lt (int .spec.terminationGracePeriodSeconds) (int $needed) -}}
{{- fail (printf "components.%s.terminationGracePeriodSeconds (%d) is below the drain the image needs (%d = drain delay + shutdown timeout + escalation)" .name (int .spec.terminationGracePeriodSeconds) (int $needed)) -}}
{{- else -}}
{{- .spec.terminationGracePeriodSeconds -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.configChecksum" -}}
{{- $parts := list (tpl (toYaml .spec.files) .root) (tpl (toYaml .spec.env) .root) (include "generic-stack.monitoringEnv" (dict "root" .root "spec" .spec)) -}}
{{- if not (or .spec.existingSecret .root.Values.global.existingSecret) -}}
{{- $parts = append $parts (tpl (toYaml .spec.secret) .root) -}}
{{- end -}}
{{- join "|" $parts | sha256sum -}}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{- define "generic-stack.probe" -}}
httpGet:
  path: {{ .cfg.path | default .path }}
  port: {{ .port }}
periodSeconds: {{ .cfg.periodSeconds | default .period }}
failureThreshold: {{ .cfg.failureThreshold }}
timeoutSeconds: {{ .cfg.timeoutSeconds }}
{{- with .cfg.initialDelaySeconds }}
initialDelaySeconds: {{ . }}
{{- end }}
{{- end -}}

{{/* ------------------------------------------------------------------------ */}}
{{/* Pod anti-affinity among the component's own replicas (hostname topology) */}}
{{- define "generic-stack.antiAffinity" -}}
{{- $term := dict "labelSelector" (dict "matchLabels" (fromYaml (include "generic-stack.selectorLabels" .))) "topologyKey" "kubernetes.io/hostname" -}}
{{- if eq .mode "required" -}}
podAntiAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    - {{ toYaml $term | nindent 6 | trim }}
{{- else -}}
podAntiAffinity:
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        {{- toYaml $term | nindent 8 }}
{{- end -}}
{{- end -}}
