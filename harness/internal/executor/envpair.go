package executor

// EnvPair is a single sandbox environment variable entry, shared by the
// container and k8s executor configs, which each convert it to their own
// native representation. It deliberately carries no executor-specific
// dependency so the factory need not import k8s client types to compose
// an env variable.
type EnvPair struct {
	Name  string
	Value string
}
