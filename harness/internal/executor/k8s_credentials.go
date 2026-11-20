package executor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// awaitCredentialPlugin runs the kubeconfig's exec credential plugin through a
// first request and fails when ctx ends before the plugin has returned. client-go
// runs the plugin without a deadline, so a hung plugin is abandoned, not
// cancelled. A context ending after the plugin returned is the caller's to report.
func awaitCredentialPlugin(ctx context.Context, restCfg *rest.Config) error {
	if restCfg.ExecProvider == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("credential plugin not started: %w", context.Cause(ctx))
	}

	// The exec authenticator wraps the transport outside any wrapper set
	// here, so a wrapper is entered only after the plugin has returned.
	var pluginReturned atomic.Bool
	probeCfg := rest.CopyConfig(restCfg)
	probeCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			pluginReturned.Store(true)
			return rt.RoundTrip(req)
		})
	})
	kube, err := kubernetes.NewForConfig(probeCfg)
	if err != nil {
		return fmt.Errorf("build kube clientset: %w", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = kube.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error()
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		if pluginReturned.Load() {
			return nil
		}
		slog.Warn("abandoned a kubeconfig exec credential plugin that did not return; its process is left to exit on its own",
			"error", context.Cause(ctx))
		return fmt.Errorf("credential plugin did not return: %w", context.Cause(ctx))
	}
}
