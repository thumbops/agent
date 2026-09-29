{{- define "thumbops-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "thumbops-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "thumbops-agent.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "thumbops-agent.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "thumbops-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "thumbops-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "thumbops-agent.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "thumbops-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Secret with the bootstrap token: the chart's own or an existing one. */}}
{{- define "thumbops-agent.bootstrapSecret" -}}
{{- if and .Values.bootstrap.token .Values.bootstrap.existingSecret }}
{{- fail "set bootstrap.token or bootstrap.existingSecret, not both" }}
{{- end }}
{{- default "thumbops-bootstrap" .Values.bootstrap.existingSecret }}
{{- end }}

{{/* The local policy as JSON; the release namespace is always denied. */}}
{{- define "thumbops-agent.policy" -}}
{{- $p := deepCopy .Values.policy }}
{{- $denied := $p.denied_namespaces | default list }}
{{- if not (has .Release.Namespace $denied) }}
{{- $denied = append $denied .Release.Namespace }}
{{- end }}
{{- $_ := set $p "denied_namespaces" $denied }}
{{- toPrettyJson $p }}
{{- end }}

{{- define "thumbops-agent.serviceEnabled" -}}
{{- if or .Values.metrics.service.enabled .Values.metrics.serviceMonitor.enabled }}true{{ end }}
{{- end }}

{{- define "thumbops-agent.subjects" -}}
- kind: ServiceAccount
  name: {{ include "thumbops-agent.fullname" . }}
  namespace: {{ .Release.Namespace }}
{{- end }}
