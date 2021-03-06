{{ define "mutatingwebhookconfiguration.yaml.tpl" }}
apiVersion: admissionregistration.k8s.io/v1beta1
kind: MutatingWebhookConfiguration
metadata:
  name: istio-sidecar-injector
  labels:
    app: {{ template "sidecar-injector.name" . }}
    chart: {{ template "sidecar-injector.chart" . }}
    heritage: {{ .Release.Service }}
    maistra-version: 1.0.10
    release: {{ .Release.Name }}
webhooks:
  - name: sidecar-injector.istio.io
    clientConfig:
      service:
        name: istio-sidecar-injector
        namespace: {{ .Release.Namespace }}
        path: "/inject"
      caBundle: ""
    rules:
      - operations: [ "CREATE" ]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["pods"]
    failurePolicy: Fail
    namespaceSelector:
{{- if .Values.enableNamespacesByDefault }}
      matchExpressions:
      - key: maistra.io/ignore-namespace
        operator: DoesNotExist
      - key: istio.openshift.com/ignore-namespace
        operator: DoesNotExist
{{- else }}
      matchLabels:
        istio-injection: enabled
{{- end }}
{{- end }}
