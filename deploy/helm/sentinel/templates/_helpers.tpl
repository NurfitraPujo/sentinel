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
Cloud provider selector. "generic" (default) and "k8s" keep the existing
behavior (SealedSecret/plain Secret, CCE Ingresses, externalServices
Endpoints). "aws" switches the secret template to an ExternalSecret synced
from SSM Parameter Store via ESO, and expects .Values.externalSecrets.secrets
to map ENV_VAR -> full SSM path. Set via global.provider (AWS values files;
K8s files set global.provider=k8s).

NOTE: .Values.global must never be dereferenced directly — Helm leaves it nil
(not an empty dict) when no `global` block exists, and chaining a field off nil
aborts rendering.
*/}}
{{- define "sentinel.provider" -}}
{{- $global := .Values.global | default dict -}}
{{- $global.provider | default .Values.provider | default "generic" -}}
{{- end -}}

{{- define "sentinel.isAws" -}}
{{- eq (include "sentinel.provider" .) "aws" -}}
{{- end -}}

{{/* ESO ClusterSecretStore name (cluster-scoped, infra-owned). */}}
{{- define "sentinel.secretStore" -}}
{{- $global := .Values.global | default dict -}}
{{- $global.secretStore | default "aws-parameter-store" -}}
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

{{/*
Renders "true" when NATS_URL is supplied via secrets instead of the ConfigMap
— e.g. an authenticated shared-cluster DSN. On AWS the ExternalSecret always
carries NATS_URL, so provider=aws short-circuits to true. On CCE it is present
only when the active pool (sealedData when backend=sealed, else data) has the
key; guards hasKey against nil maps.
*/}}
{{- define "sentinel.natsUrlIsSecret" -}}
{{- if eq (include "sentinel.isAws" .) "true" -}}
true
{{- else -}}
{{- $sealed := .Values.secrets.sealedData | default dict -}}
{{- $plain := .Values.secrets.data | default dict -}}
{{- if or (hasKey $sealed "NATS_URL") (hasKey $plain "NATS_URL") }}true{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Renders "true" when `key` exists in the ACTIVE secret pool. The pool is:
  - AWS  : .Values.externalSecrets.secrets (ENV_VAR -> SSM path)
  - CCE  : .Values.secrets.sealedData when backend=sealed, else .Values.secrets.data
Call with (list $ "KEY"). Used by optional keys (Google OAuth) that must not
produce a CreateContainerConfigError when the key is absent.
*/}}
{{- define "sentinel.secretPoolContains" -}}
{{- $root := index . 0 -}}
{{- $key := index . 1 -}}
{{- $pool := dict -}}
{{- if eq (include "sentinel.isAws" $root) "true" -}}
{{- $pool = $root.Values.externalSecrets.secrets | default dict -}}
{{- else if eq ($root.Values.secrets.backend | default "plain") "sealed" -}}
{{- $pool = $root.Values.secrets.sealedData | default dict -}}
{{- else -}}
{{- $pool = $root.Values.secrets.data | default dict -}}
{{- end -}}
{{- if hasKey $pool $key }}true{{- end -}}
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

{{/*
sentinel.migratePostgresEnv -- the connection the MIGRATION job uses.

Identical to sentinel.postgresEnv unless .Values.migrate.postgres.user is set,
in which case POSTGRES_USER and POSTGRES_PASSWORD name the owning role instead.
See the comment on migrate.postgres in values.yaml for why this differs per
environment (report-an-issue/BUG-68).
*/}}
{{- define "sentinel.migratePostgresEnv" -}}
{{- $m := (.Values.migrate | default dict).postgres | default dict }}
{{- $user := $m.user | default "" }}
{{- if not $user }}
{{- include "sentinel.postgresEnv" . }}
{{- else }}
{{- $key := $m.passwordSecretKey | default "" }}
{{- if not $key }}
{{- fail "migrate.postgres.passwordSecretKey must name the Secret key holding migrate.postgres.user's password" }}
{{- end }}
{{- if eq (include "sentinel.isAws" .) "true" }}
{{- $ext := (.Values.externalSecrets | default dict).secrets | default dict }}
{{- if not (hasKey $ext $key) }}
{{- fail (printf "migrate.postgres.passwordSecretKey %s is not in externalSecrets.secrets, so the Secret would never carry it" $key) }}
{{- end }}
{{- end }}
- name: POSTGRES_HOST
  value: {{ include "sentinel.postgresHost" . | quote }}
- name: POSTGRES_PORT
  value: {{ .Values.config.postgres.port | quote }}
- name: POSTGRES_USER
  value: {{ $user | quote }}
- name: POSTGRES_DB
  value: {{ .Values.config.postgres.database | quote }}
- name: POSTGRES_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "sentinel.secretName" . }}
      key: {{ $key | quote }}
{{- end }}
{{- end -}}

{{- define "sentinel.otelEnv" -}}
{{- if .Values.config.otel.exporterEndpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.config.otel.exporterEndpoint | quote }}
{{- end }}
{{- end -}}

{{/*
sentinel.externalSecretsRefreshInterval -- how often ESO re-reads SSM.

MUST NOT BE "0" (report-an-issue/BUG-90). An ExternalSecret with refreshInterval
"0" syncs ONCE, when it is created, and never again. The comment this replaces
said "Refresh driven by ArgoCD sync", and that is false: Argo applying a changed
spec advances metadata.generation without ESO re-reading anything, while the
Ready condition goes on reporting True/SecretSynced. Read status.refreshTime.

This object is less visibly stale than its siblings only because sentinel is
redeployed often -- measured 2026-10-03, 6.8h on staging and 17.3h on
production. Recreation on deploy is not refreshing; it is the one event that
happens to re-read SSM, so between deploys a rotated or newly added parameter
never arrives.

The external-secrets.io/force-sync annotation does not help. The only manual
lever is deleting the owned target Secret, and consumers then need a restart
because the Secret is read at process start.

Override with global.externalSecretsRefreshInterval for a slower cadence. Note
what a real interval also means: a parameter DELETED from SSM now disappears
from the Secret within one interval instead of lingering until a recreate.
*/}}
{{- define "sentinel.externalSecretsRefreshInterval" -}}
{{- $v := "" -}}
{{- if .Values.global -}}
{{- /* NO `default` BEFORE THE CHECK. Helm's `default` treats 0 and "0" as empty,
       so `$v | default ""` would silently turn an explicit 0 into "" and the
       guard below could never fire -- a check that cannot fail, which is the
       same shape as the bug it guards. Read it raw, judge it, then default. */ -}}
{{- $v = .Values.global.externalSecretsRefreshInterval -}}
{{- end -}}
{{- if and (ne (toString $v) "<nil>") (ne (toString $v) "") -}}
{{- if or (eq (toString $v) "0") (eq (toString $v) "0s") -}}
{{- fail "global.externalSecretsRefreshInterval must not be 0: an ExternalSecret with refreshInterval 0 syncs once at creation and never again (report-an-issue/BUG-90)" -}}
{{- end -}}
{{- toString $v -}}
{{- else -}}
1h
{{- end -}}
{{- end -}}
