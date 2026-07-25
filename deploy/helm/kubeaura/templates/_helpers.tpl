{{- define "kubeaura.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kubeaura.fullname" -}}
{{- $name := include "kubeaura.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kubeaura.labels" -}}
app.kubernetes.io/name: {{ include "kubeaura.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "kubeaura.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubeaura.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kubeaura.serviceAccountName" -}}
{{ include "kubeaura.fullname" . }}
{{- end -}}

{{- define "kubeaura.secretName" -}}
{{- if .Values.ai.existingSecret -}}
{{ .Values.ai.existingSecret }}
{{- else -}}
{{ include "kubeaura.fullname" . }}
{{- end -}}
{{- end -}}
