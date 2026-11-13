# Executor test fixtures

- `nitro-admitted-pod.json` is modelled on Pods read back from EKS
  (Kubernetes 1.36) after they were Ready on the 2026-11-11 EKS run
  ([status](../../../../docs/executors/k8s.md#status)), with
  identifiers rewritten to the tests' names. It is not a `Create` response.
  Its `enableServiceLinks` is `false`, the value the executor submits; every
  other value follows those Pods, including what EKS and API defaulting
  added: `1Gi` for a submitted `1024Mi`, topology labels, the merged node
  selector, the default tolerations, the overhead and the scheduling fields.
  `decodeAdmittedNitroPod` in `k8s_admission_strict_test.go` decodes it.
- `runtime-diagnostics-ordinary-pod.txt` is the output of the runtime
  diagnostics' guest-file script in an ordinary BusyBox Pod on a local `kind`
  cluster: DMI product name `kind`, interfaces `lo` and `eth0`, three routes.
