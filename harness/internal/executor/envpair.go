package executor

// EnvPair is a single sandbox environment variable entry, shared by
// ContainerExecutorConfig.ExtraEnv and K8sExecutorConfig.ExtraEnv (issue
// #516). Each executor config converts it to its own native env
// representation ("KEY=VALUE" strings for the container executor,
// corev1.EnvVar for the k8s family) — this type deliberately carries no
// executor-specific dependency so callers outside this package (the
// factory) need not import k8s client types just to compose an env
// variable.
type EnvPair struct {
	Name  string
	Value string
}
