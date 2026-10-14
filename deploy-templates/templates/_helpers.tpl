{{/*
Expand the name of the chart.
*/}}
{{- define "krci-env-daemon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "krci-env-daemon.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "krci-env-daemon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "krci-env-daemon.labels" -}}
helm.sh/chart: {{ include "krci-env-daemon.chart" . }}
{{ include "krci-env-daemon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "krci-env-daemon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "krci-env-daemon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "krci-env-daemon.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "krci-env-daemon.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Namespaces the daemon watches when it does not watch all of them: the environments' namespaces,
or its own namespace when no environment is listed yet.
*/}}
{{- define "krci-env-daemon.watchNamespaces" -}}
{{- $namespaces := list }}
{{- range .Values.environments }}
{{- $namespaces = append $namespaces .namespace }}
{{- end }}
{{- if not $namespaces }}
{{- $namespaces = list .Release.Namespace }}
{{- end }}
{{- join "," $namespaces }}
{{- end }}

{{/*
Fails on environments the templates cannot render: a missing or repeated namespace, or a
waiting page route without the hostnames that carry its /.krci-wake/ path.
*/}}
{{- define "krci-env-daemon.validateEnvironments" -}}
{{- $seen := list }}
{{- range .Values.environments }}
{{- $ns := required "every environment needs a namespace" .namespace }}
{{- if has $ns $seen }}
{{- fail (printf "environment namespace %s is listed twice" $ns) }}
{{- end }}
{{- if and .route (not .hostnames) }}
{{- fail (printf "environment %s: a route needs hostnames for the waiting page's /.krci-wake/ path" $ns) }}
{{- end }}
{{- $seen = append $seen $ns }}
{{- end }}
{{- end }}

{{/*
The objects the daemon reads: list and watch only, for its informers.
*/}}
{{- define "krci-env-daemon.readRules" -}}
- apiGroups: ["keda.sh"]
  resources: ["scaledobjects"]
  verbs: ["list", "watch"]
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["list", "watch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list", "watch"]
- apiGroups: ["gateway.networking.k8s.io"]
  resources: ["httproutes"]
  verbs: ["list", "watch"]
{{- end }}
