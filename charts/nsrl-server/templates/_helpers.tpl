{{- define "nsrl-server.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nsrl-server.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "nsrl-server.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "nsrl-server.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "nsrl-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "nsrl-server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nsrl-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "nsrl-server.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "nsrl-server.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "nsrl-server.claimName" -}}
{{- default (include "nsrl-server.fullname" .) .Values.persistence.existingClaim }}
{{- end }}
