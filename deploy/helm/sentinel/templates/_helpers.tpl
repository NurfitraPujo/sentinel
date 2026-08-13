{{/*
Common name helpers.
*/}}
{{- define "sentinel.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sentinel.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "sentinel.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "sentinel.labels" -}}
app.kubernetes.io/name: {{ include "sentinel.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "sentinel.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "sentinel.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Secret name — either the pre-provisioned one or the chart-managed one.
*/}}
{{- define "sentinel.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "sentinel.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "sentinel.configMapName" -}}
{{- printf "%s-config" (include "sentinel.fullname" .) -}}
{{- end -}}

{{/*
Resolved service hostnames. Bundled dependency service names are derived from the fullname;
external hosts come from config.* when provided.
*/}}
{{- define "sentinel.postgresHost" -}}
{{- if .Values.config.postgres.host -}}
{{- .Values.config.postgres.host -}}
{{- else -}}
{{- printf "%s-postgres" (include "sentinel.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "sentinel.natsUrl" -}}
{{- if .Values.config.nats.url -}}
{{- .Values.config.nats.url -}}
{{- else -}}
{{- printf "nats://%s-nats:4222" (include "sentinel.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "sentinel.redisAddr" -}}
{{- if .Values.config.redis.addr -}}
{{- .Values.config.redis.addr -}}
{{- else -}}
{{- printf "%s-redis:6379" (include "sentinel.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "sentinel.s3Endpoint" -}}
{{- if .Values.config.s3.endpoint -}}
{{- .Values.config.s3.endpoint -}}
{{- else -}}
{{- printf "http://%s-minio:9000" (include "sentinel.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Shared POSTGRES_* env block (host/port/user/db as literals; password from Secret).
*/}}
{{- define "sentinel.postgresEnv" -}}
- name: POSTGRES_HOST
  value: {{ include "sentinel.postgresHost" . | quote }}
- name: POSTGRES_PORT
  value: {{ .Values.config.postgres.port | quote }}
- name: POSTGRES_USER
  value: {{ .Values.config.postgres.user | quote }}
- name: POSTGRES_DB
  value: {{ .Values.config.postgres.database | quote }}
- name: POSTGRES_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "sentinel.secretName" . }}
      key: POSTGRES_PASSWORD
{{- end -}}

{{- define "sentinel.otelEnv" -}}
{{- if .Values.config.otel.exporterEndpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.config.otel.exporterEndpoint | quote }}
{{- end }}
{{- end -}}
