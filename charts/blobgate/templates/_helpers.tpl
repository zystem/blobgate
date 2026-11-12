{{- define "blobgate.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "blobgate.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}{{- .Release.Name | trunc 63 | trimSuffix "-" -}}{{- else -}}{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}{{- end -}}
{{- end -}}
{{- end -}}

{{- define "blobgate.labels" -}}
app.kubernetes.io/name: {{ include "blobgate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "blobgate.selector" -}}
app.kubernetes.io/name: {{ include "blobgate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "blobgate.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/* the Secret that holds the module's secrets */}}
{{- define "blobgate.secret" -}}
{{- default (include "blobgate.fullname" .) .Values.existingSecret -}}
{{- end -}}

{{/* <shard>=<secret>;<shard>=<secret> */}}
{{- define "blobgate.shards" -}}
{{- $parts := list -}}
{{- range $shard := (keys .Values.secrets.shards | sortAlpha) -}}
{{- $parts = append $parts (printf "%s=%s" $shard (index $.Values.secrets.shards $shard)) -}}
{{- end -}}
{{- join ";" $parts -}}
{{- end -}}

{{/* an environment variable that is set only when its value is not the program's default (0, "" or false) */}}
{{- define "blobgate.optEnv" -}}
{{- if .value }}
- {name: {{ .name }}, value: {{ .value | toString | quote }}}
{{- end }}
{{- end -}}
