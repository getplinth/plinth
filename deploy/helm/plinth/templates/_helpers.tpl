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

{{/* Fail early, with a sentence that says what to set, instead of crash-looping later. */}}
{{- define "plinth.validate" -}}
{{- $oidc := .Values.config.oidc -}}
{{- if not $oidc.issuer -}}{{ fail "config.oidc.issuer is required: the URL of your OpenID Connect identity provider" }}{{- end -}}
{{- if not $oidc.audience -}}{{ fail "config.oidc.audience is required: the client id registered for Plinth at your identity provider" }}{{- end -}}
{{- if and (not (hasPrefix "https://" $oidc.issuer)) (not $oidc.allowHttp) -}}
{{ fail (printf "config.oidc.issuer %q must be https. Set config.oidc.allowHttp=true only for local testing." $oidc.issuer) }}
{{- end -}}
{{- if and .Values.database.existingSecret .Values.database.url -}}{{ fail "set either database.existingSecret or database.url, not both" }}{{- end -}}
{{- if not (or .Values.database.existingSecret .Values.database.url) -}}{{ fail "the database connection is required: set database.existingSecret (a Secret holding the PostgreSQL URL, recommended) or database.url" }}{{- end -}}
{{- if and .Values.masterKey.existingSecret .Values.masterKey.value -}}{{ fail "set either masterKey.existingSecret or masterKey.value, not both" }}{{- end -}}
{{- if not (or .Values.masterKey.existingSecret .Values.masterKey.value) -}}{{ fail "the master key is required: set masterKey.existingSecret (a Secret holding 32 random bytes, base64) or masterKey.value. Create one with `plinth genkey`. Losing it makes stored integrations unreadable." }}{{- end -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.hosts) -}}{{ fail "ingress.enabled needs at least one entry in ingress.hosts" }}{{- end -}}
{{- end -}}
