package executor

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// awaitCredentialPlugin makes the first authenticated request of a client
// built from restCfg and returns an error when ctx ends before the kubeconfig's
// exec credential plugin has returned. client-go runs the plugin without a
// deadline and without the request context, so a hung plugin cannot be
// cancelled: the request is abandoned and the plugin process is left to exit
// on its own. Credentials are cached after the plugin returns, so later
// requests wait for it again only once they expire. It does nothing for a
// config without an exec plugin, and a plugin that fails is not reported here:
// the next request runs it again and returns its error.
func awaitCredentialPlugin(ctx context.Context, restCfg *rest.Config, kube kubernetes.Interface) error {
	if restCfg.ExecProvider == nil {
		return nil
	}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_, _ = kube.Discovery().ServerVersion()
	}()
	select {
	case <-returned:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("credential plugin did not return: %w", context.Cause(ctx))
	}
}
