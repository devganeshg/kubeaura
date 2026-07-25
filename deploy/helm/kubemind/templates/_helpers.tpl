{{- define "kubemind.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kubemind.fullname" -}}
{{- $name := include "kubemind.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kubemind.labels" -}}
app.kubernetes.io/name: {{ include "kubemind.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "kubemind.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubemind.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kubemind.serviceAccountName" -}}
{{ include "kubemind.fullname" . }}
{{- end -}}

{{- define "kubemind.secretName" -}}
{{- if .Values.ai.existingSecret -}}
{{ .Values.ai.existingSecret }}
{{- else -}}
{{ include "kubemind.fullname" . }}
{{- end -}}
{{- end -}}
