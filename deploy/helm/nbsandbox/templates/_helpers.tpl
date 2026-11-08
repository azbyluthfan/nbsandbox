{{/*
Copyright 2026 The nbsandbox Authors
SPDX-License-Identifier: Apache-2.0
*/}}
{{- define "nbsandbox.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}{{ .Release.Name | trunc 50 | trimSuffix "-" }}{{ else }}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 50 | trimSuffix "-" }}{{ end }}
{{- end }}

{{- define "nbsandbox.labels" -}}
app.kubernetes.io/name: nbsandbox
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "nbsandbox.selector" -}}
app.kubernetes.io/name: nbsandbox
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "nbsandbox.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/* The container hardening both kinds of process get. */}}
{{- define "nbsandbox.containerSecurity" -}}
runAsNonRoot: true
runAsUser: 1000
runAsGroup: 1000
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: { drop: [ALL] }
seccompProfile: { type: RuntimeDefault }
{{- end }}

{{/* Environment both kinds of process share: settings and secrets. */}}
{{- define "nbsandbox.env" -}}
- { name: NBSANDBOX_CONFIG, value: /etc/nbsandbox/settings.yaml }
- name: NBSANDBOX_DATABASE_URL
  valueFrom: { secretKeyRef: { name: {{ required "database.existingSecret is required" .Values.database.existingSecret | quote }}, key: {{ .Values.database.key | quote }} } }
- { name: NBSANDBOX_WORKER_ID, valueFrom: { fieldRef: { fieldPath: metadata.name } } }
{{- if and (eq .Values.storage.kind "s3") .Values.storage.s3.existingSecret }}
- name: NBSANDBOX_S3_ACCESS_KEY
  valueFrom: { secretKeyRef: { name: {{ .Values.storage.s3.existingSecret | quote }}, key: access-key } }
- name: NBSANDBOX_S3_SECRET_KEY
  valueFrom: { secretKeyRef: { name: {{ .Values.storage.s3.existingSecret | quote }}, key: secret-key } }
{{- end }}
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "nbsandbox.volumeMounts" -}}
- { name: settings, mountPath: /etc/nbsandbox/settings.yaml, subPath: settings.yaml, readOnly: true }
- { name: data, mountPath: /srv/nbsandbox }
{{- if .Values.auth.basic }}
- { name: users, mountPath: /etc/nbsandbox/users.yaml, subPath: users.yaml, readOnly: true }
{{- end }}
{{- end }}

{{- define "nbsandbox.volumes" -}}
- name: settings
  configMap: { name: {{ include "nbsandbox.fullname" . }} }
{{- if .Values.auth.basic }}
- name: users
  secret: { secretName: {{ required "auth.usersSecret is required with auth.basic" .Values.auth.usersSecret | quote }}, defaultMode: 0400 }
{{- end }}
{{- end }}

{{- define "nbsandbox.probes" -}}
readinessProbe:
  httpGet: { path: /readyz, port: http }
  periodSeconds: 5
  failureThreshold: 3
livenessProbe:
  httpGet: { path: /healthz, port: http }
  periodSeconds: 10
  failureThreshold: 6
{{- end }}
