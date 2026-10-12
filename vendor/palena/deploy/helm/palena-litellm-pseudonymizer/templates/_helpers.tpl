{{/* Expand the name of the chart. */}}
{{- define "pseudonymizer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "pseudonymizer.fullname" -}}
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

{{- define "pseudonymizer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "pseudonymizer.labels" -}}
helm.sh/chart: {{ include "pseudonymizer.chart" . }}
{{ include "pseudonymizer.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Selector labels shared by EVERY object in the release. */}}
{{- define "pseudonymizer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pseudonymizer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Selector labels for the pseudonymizer service ITSELF.

The bundled dependencies (redis, presidio-analyzer, presidio-image-redactor) all
carry the shared selectorLabels plus their own app.kubernetes.io/component, so a
selector built from selectorLabels alone matches EVERY pod in the release, not
just ours. That is not cosmetic: the main Service then load-balances across
presidio and redis, which answer 404 (or nothing) for /v1/waeg/check and
/beta/litellm_basic_guardrail_api. A gateway with the guardrail set to fail
closed blocks those requests outright — measured at roughly 40% of calls with the
default 2 replicas.

Anything that must address only the pseudonymizer pods uses this instead.
*/}}
{{- define "pseudonymizer.componentSelectorLabels" -}}
{{ include "pseudonymizer.selectorLabels" . }}
app.kubernetes.io/component: pseudonymizer
{{- end -}}

{{- define "pseudonymizer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pseudonymizer.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Component names for the bundled dependencies. */}}
{{- define "pseudonymizer.redis.fullname" -}}
{{- printf "%s-redis" (include "pseudonymizer.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "pseudonymizer.analyzer.fullname" -}}
{{- printf "%s-presidio-analyzer" (include "pseudonymizer.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "pseudonymizer.imageRedactor.fullname" -}}
{{- printf "%s-presidio-image-redactor" (include "pseudonymizer.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Resolved Redis URL: bundled service name or the external url. */}}
{{- define "pseudonymizer.redisURL" -}}
{{- if .Values.redis.enabled -}}
{{- printf "redis://%s:6379/0" (include "pseudonymizer.redis.fullname" .) -}}
{{- else -}}
{{- required "redis.url is required when redis.enabled=false" .Values.redis.url -}}
{{- end -}}
{{- end -}}

{{/* Resolved Presidio Analyzer URL. Bundled service listens on 3000. */}}
{{- define "pseudonymizer.analyzerURL" -}}
{{- if .Values.presidio.analyzer.enabled -}}
{{- printf "http://%s:3000" (include "pseudonymizer.analyzer.fullname" .) -}}
{{- else -}}
{{- required "presidio.analyzer.url is required when presidio.analyzer.enabled=false" .Values.presidio.analyzer.url -}}
{{- end -}}
{{- end -}}

{{/* Resolved Presidio Image Redactor URL. */}}
{{- define "pseudonymizer.imageRedactorURL" -}}
{{- if .Values.presidio.imageRedactor.enabled -}}
{{- printf "http://%s:3000" (include "pseudonymizer.imageRedactor.fullname" .) -}}
{{- else -}}
{{- required "presidio.imageRedactor.url is required when presidio.imageRedactor.enabled=false" .Values.presidio.imageRedactor.url -}}
{{- end -}}
{{- end -}}

{{/* Whether the analyzer needs a mounted config (NER-ORG on, a deny-list, or more than English). */}}
{{- define "pseudonymizer.analyzer.needsConfig" -}}
{{- if or .Values.presidio.analyzer.nerOrganization (include "pseudonymizer.analyzer.needsRecognizers" .) (include "pseudonymizer.multilingual" .) -}}
true
{{- end -}}
{{- end -}}

{{/*
Languages Presidio analyzes in, comma-separated: config.languages, else
config.language. Each must have a bundled spaCy model (see spacyModel).
*/}}
{{- define "pseudonymizer.languages" -}}
{{- $langs := .Values.config.languages | default (list .Values.config.language) -}}
{{- $out := list -}}
{{- range $langs -}}
{{- $l := . | toString | trim | lower -}}
{{- if not (hasKey (include "pseudonymizer.spacyModels" $ | fromYaml) $l) -}}
{{- fail (printf "config.languages: no spaCy model is bundled for %q (supported: en, de, fr, it)" $l) -}}
{{- end -}}
{{- if not (has $l $out) -}}
{{- $out = append $out $l -}}
{{- end -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}

{{/* The spaCy model per supported language (the multilingual analyzer image ships all four). */}}
{{- define "pseudonymizer.spacyModels" -}}
en: en_core_web_lg
de: de_core_news_lg
fr: fr_core_news_lg
it: it_core_news_lg
{{- end -}}

{{/* True when Presidio must analyze in anything other than English alone. */}}
{{- define "pseudonymizer.multilingual" -}}
{{- if ne (include "pseudonymizer.languages" .) "en" -}}
true
{{- end -}}
{{- end -}}

{{/* True when the recognizer registry must be mounted (deny-lists or extra languages). */}}
{{- define "pseudonymizer.analyzer.needsRecognizers" -}}
{{- if or (gt (len .Values.presidio.analyzer.organizations) 0) (gt (len .Values.presidio.analyzer.denyLists) 0) (include "pseudonymizer.multilingual" .) -}}
true
{{- end -}}
{{- end -}}

{{/*
Analyzer image: the stock Presidio image ships English only, so any other
language switches to the multilingual image (same Presidio plus the de/fr/it
models).
*/}}
{{- define "pseudonymizer.analyzer.image" -}}
{{- if include "pseudonymizer.multilingual" . -}}
{{- $m := .Values.presidio.analyzer.multilingualImage -}}
{{- printf "%s:%s" $m.repository ($m.tag | default .Chart.AppVersion) -}}
{{- else -}}
{{- .Values.presidio.analyzer.image -}}
{{- end -}}
{{- end -}}

{{/* A YAML block list ("  - a\n  - b") of the given strings. */}}
{{- define "pseudonymizer.yamlList" -}}
{{- $items := list -}}
{{- range . -}}
{{- $items = append $items (printf "  - %s" .) -}}
{{- end -}}
{{- join "\n" $items -}}
{{- end -}}
