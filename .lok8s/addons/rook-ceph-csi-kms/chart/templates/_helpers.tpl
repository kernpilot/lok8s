{{- /*
rook-ceph-csi-kms.binding: the bindings of one role of rbac.yaml, with the
name of the role. Takes a dict:
  - kind: Role or ClusterRole (the kind of the role);
  - name: the name of the role;
  - option: the value of the csi-kms.lok8s.dev/option label;
  - plugins: a list of "node" and "ctrl" (the RBD node plugin and
    controller plugin ServiceAccounts that the rook-ceph addon creates);
  - namespaces: the tenantNamespaces, for a ClusterRole of the PVC
    namespace only (else empty).
A Role gets a RoleBinding in rook-ceph. A ClusterRole gets one RoleBinding
in each of namespaces, or a ClusterRoleBinding when namespaces is empty.
*/ -}}
{{- define "rook-ceph-csi-kms.binding" -}}
{{- $b := . }}
{{- if and (eq $b.kind "ClusterRole") $b.namespaces }}
{{- range $b.namespaces }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ $b.name }}
  namespace: {{ . }}
  labels:
    csi-kms.lok8s.dev/option: {{ $b.option }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ $b.name }}
subjects:
  {{- range $b.plugins }}
  - kind: ServiceAccount
    name: rook-ceph-rbd-csi-ceph-com-{{ . }}plugin-sa
    namespace: rook-ceph
  {{- end }}
{{- end }}
{{- else }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: {{ $b.kind }}Binding
metadata:
  name: {{ $b.name }}
  {{- if eq $b.kind "Role" }}
  namespace: rook-ceph
  {{- end }}
  labels:
    csi-kms.lok8s.dev/option: {{ $b.option }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: {{ $b.kind }}
  name: {{ $b.name }}
subjects:
  {{- range $b.plugins }}
  - kind: ServiceAccount
    name: rook-ceph-rbd-csi-ceph-com-{{ . }}plugin-sa
    namespace: rook-ceph
  {{- end }}
{{- end }}
{{- end }}
