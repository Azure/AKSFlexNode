{{/*
Upstream components and the CLI assume unbounded-system. Install the extension
with --release-namespace unbounded-system; any other namespace fails render.
*/}}
{{- define "flex-operator.namespace" -}}
{{- if ne .Release.Namespace "unbounded-system" -}}
{{- fail (printf "flex-operator must be installed into release namespace unbounded-system, got %q" .Release.Namespace) -}}
{{- end -}}
{{- .Release.Namespace -}}
{{- end -}}

{{- define "flex-operator.labels" -}}
app.kubernetes.io/name: unbounded-operator
app.kubernetes.io/component: controller
app.kubernetes.io/part-of: flex-operator
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "flex-operator.image" -}}
{{- $nameTag := default (printf "unbounded-operator:%s" .Chart.AppVersion) .Values.operator.image -}}
{{- printf "%s/%s" (trimSuffix "/" .Values.operator.imageRegistry) $nameTag -}}
{{- end -}}

{{- define "flex-operator.configData" -}}
UNBOUNDED_API_SERVER_ENDPOINT: {{ .Values.operator.apiServerEndpoint | quote }}
UNBOUNDED_IMAGE_REGISTRY: {{ trimSuffix "/" .Values.operator.imageRegistry | quote }}
UNBOUNDED_REAP_LEGACY_RESOURCES: {{ .Values.operator.reapLegacyResources | toString | quote }}
{{- end -}}
