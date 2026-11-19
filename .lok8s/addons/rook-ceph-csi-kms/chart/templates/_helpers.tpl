{{- /*
rook-ceph-csi-kms.binding: the binding of one role of rbac.yaml, with the
same name and kind (Role: a RoleBinding in rook-ceph; ClusterRole: a
ClusterRoleBinding). Takes a dict: kind, name, plugins (a list of "node"
and "ctrl": the RBD node plugin and controller plugin ServiceAccounts that
the rook-ceph addon creates).
*/ -}}
{{- define "rook-ceph-csi-kms.binding" -}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: {{ .kind }}Binding
metadata:
  name: {{ .name }}
  {{- if eq .kind "Role" }}
  namespace: rook-ceph
  {{- end }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: {{ .kind }}
  name: {{ .name }}
subjects:
  {{- range .plugins }}
  - kind: ServiceAccount
    name: rook-ceph-rbd-csi-ceph-com-{{ . }}plugin-sa
    namespace: rook-ceph
  {{- end }}
{{- end }}
