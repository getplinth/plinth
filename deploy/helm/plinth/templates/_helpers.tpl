{{/* Name of the chart, trimmed to what Kubernetes allows. */}}
{{- define "plinth.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Name of every object: the release name, unless it already contains the chart name. */}}
{{- define "plinth.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "plinth.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "plinth.selectorLabels" -}}
app.kubernetes.io/name: {{ include "plinth.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "plinth.labels" -}}
helm.sh/chart: {{ include "plinth.chart" . }}
{{ include "plinth.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "plinth.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "plinth.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Image reference: a digest wins over a tag; the tag defaults to the chart's appVersion. */}}
{{- define "plinth.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
Where each secret value comes from. The chart can create a Secret from values
(convenient, but the value then sits in Helm's release data), or point at a
Secret you manage (recommended). Exactly one source must be chosen for each.
*/}}
{{- define "plinth.databaseSecretName" -}}
{{- if .Values.database.existingSecret -}}{{ .Values.database.existingSecret }}{{- else -}}{{ include "plinth.fullname" . }}{{- end -}}
{{- end -}}
{{- define "plinth.databaseSecretKey" -}}
{{- if .Values.database.existingSecret -}}{{ .Values.database.existingSecretKey }}{{- else -}}database-url{{- end -}}
{{- end -}}
{{- define "plinth.masterKeySecretName" -}}
{{- if .Values.masterKey.existingSecret -}}{{ .Values.masterKey.existingSecret }}{{- else -}}{{ include "plinth.fullname" . }}{{- end -}}
{{- end -}}
{{- define "plinth.masterKeySecretKey" -}}
{{- if .Values.masterKey.existingSecret -}}{{ .Values.masterKey.existingSecretKey }}{{- else -}}master-key{{- end -}}
{{- end -}}

{{/* Where people open Plinth: config.publicUrl, else the first TLS ingress host, else nothing. */}}
{{- define "plinth.publicUrl" -}}
{{- if .Values.config.publicUrl -}}{{ .Values.config.publicUrl | trimSuffix "/" }}
{{- else if and .Values.ingress.enabled .Values.ingress.hosts .Values.ingress.tls -}}https://{{ first .Values.ingress.hosts }}
{{- end -}}
{{- end -}}

{{- define "plinth.adminSecretName" -}}
{{- if .Values.admin.existingSecret -}}{{ .Values.admin.existingSecret }}{{- else -}}{{ include "plinth.fullname" . }}-admin{{- end -}}
{{- end -}}
{{- define "plinth.adminSecretKey" -}}
{{- if .Values.admin.existingSecret -}}{{ .Values.admin.existingSecretKey }}{{- else -}}password{{- end -}}
{{- end -}}

{{/* Fail early, with a sentence that says what to set, instead of crash-looping later. */}}
{{- define "plinth.validate" -}}
{{- $oidc := .Values.config.oidc -}}
{{- if and $oidc.issuer (not $oidc.audience) -}}{{ fail "config.oidc.audience is required with config.oidc.issuer: the client id registered for Plinth at your identity provider" }}{{- end -}}
{{- if and $oidc.audience (not $oidc.issuer) -}}{{ fail "config.oidc.issuer is required with config.oidc.audience: the URL of your OpenID Connect identity provider. Or leave both empty and add the provider in the control panel." }}{{- end -}}
{{- if and $oidc.issuer (not (hasPrefix "https://" $oidc.issuer)) (not $oidc.allowHttp) -}}
{{ fail (printf "config.oidc.issuer %q must be https. Set config.oidc.allowHttp=true only for local testing." $oidc.issuer) }}
{{- end -}}
{{- if and (not .Values.admin.create) (not $oidc.issuer) -}}{{ fail "nobody could sign in: set admin.create=true (the chart creates the first administrator), or config.oidc.issuer and config.oidc.audience" }}{{- end -}}
{{- if and .Values.database.existingSecret .Values.database.url -}}{{ fail "set either database.existingSecret or database.url, not both" }}{{- end -}}
{{- if not (or .Values.database.existingSecret .Values.database.url) -}}{{ fail "the database connection is required: set database.existingSecret (a Secret holding the PostgreSQL URL, recommended) or database.url" }}{{- end -}}
{{- if and .Values.masterKey.existingSecret .Values.masterKey.value -}}{{ fail "set either masterKey.existingSecret or masterKey.value, not both" }}{{- end -}}
{{- if not (or .Values.masterKey.existingSecret .Values.masterKey.value) -}}{{ fail "the master key is required: set masterKey.existingSecret (a Secret holding 32 random bytes, base64) or masterKey.value. Create one with `plinth genkey`. Losing it makes stored integrations unreadable." }}{{- end -}}
{{- $n := .Values.notifications -}}
{{- if and $n.smtp.host (not $n.smtp.from) -}}{{ fail "notifications.smtp.from is required with notifications.smtp.host: the sender address, for example Plinth <plinth@example.com>" }}{{- end -}}
{{- if and (not $n.smtp.host) (or $n.smtp.from $n.smtp.existingSecret) -}}{{ fail "notifications.smtp.host is required when other notifications.smtp settings are given" }}{{- end -}}
{{- if and (eq $n.smtp.tls "none") (not $n.allowInsecure) -}}{{ if $n.smtp.host }}{{ fail "notifications.smtp.tls=none sends mail and the password unencrypted: it also needs notifications.allowInsecure=true, and is for testing only" }}{{ end }}{{- end -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.hosts) -}}{{ fail "ingress.enabled needs at least one entry in ingress.hosts" }}{{- end -}}
{{- end -}}
