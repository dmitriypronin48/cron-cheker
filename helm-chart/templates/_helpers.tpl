{{- define "cron-checker-server.name" -}}
cron-checker-server
{{- end -}}

{{- define "cron-checker-server.fullname" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cron-checker-server.labels" -}}
app.kubernetes.io/name: {{ include "cron-checker-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
