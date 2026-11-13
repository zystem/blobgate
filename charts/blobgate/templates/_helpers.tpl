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

{{/* the tenants and their secrets: `secrets.tenants`, or `secrets.shards`, the name of the first version */}}
{{- define "blobgate.tenantMap" -}}
{{- if .Values.secrets.tenants -}}{{- toJson .Values.secrets.tenants -}}{{- else -}}{{- toJson .Values.secrets.shards -}}{{- end -}}
{{- end -}}

{{/* <tenant>=<secret>;<tenant>=<secret> */}}
{{- define "blobgate.tenants" -}}
{{- $m := include "blobgate.tenantMap" . | fromJson -}}
{{- $parts := list -}}
{{- range $t := (keys $m | sortAlpha) -}}
{{- $parts = append $parts (printf "%s=%s" $t (index $m $t)) -}}
{{- end -}}
{{- join ";" $parts -}}
{{- end -}}

{{/* an environment variable that is set only when its value is not the program's default (0, "" or false) */}}
{{- define "blobgate.optEnv" -}}
{{- if .value }}
- {name: {{ .name }}, value: {{ .value | toString | quote }}}
{{- end }}
{{- end -}}
