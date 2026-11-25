package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const alphaSandboxResourceList = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"agents.x-k8s.io/v1alpha1","resources":[{"name":"sandboxes","namespaced":true,"kind":"Sandbox","verbs":["get"]}]}`

// discoveryClient returns a real clientset, so discovery goes through
// client-go's own status mapping, talking to a server that runs handler.
func discoveryClient(t *testing.T, handler http.HandlerFunc) kubernetes.Interface {
	t.Helper()
	must := require.New(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	must.NoError(err)
	return cs
}

func serveAlphaOnly(t *testing.T, otherwise http.HandlerFunc) (http.HandlerFunc, *bool) {
	t.Helper()
	alphaRequested := false
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/agents.x-k8s.io/v1alpha1" {
			alphaRequested = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(alphaSandboxResourceList))
			return
		}
		otherwise(w, r)
	}, &alphaRequested
}

func TestResolveSandboxGVR_RealDiscovery_PlainText404FallsBack(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	handler, _ := serveAlphaOnly(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	})
	cs := discoveryClient(t, handler)

	got, err := resolveSandboxGVR(context.Background(), cs.Discovery())

	must.NoError(err)
	check.Equal(testSandboxGVRAlpha, got)
}

func TestResolveSandboxGVR_RealDiscovery_OtherStatusesDoNotFallBack(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			handler, alphaRequested := serveAlphaOnly(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "refused", code)
			})
			cs := discoveryClient(t, handler)

			_, err := resolveSandboxGVR(context.Background(), cs.Discovery())

			must.Error(err)
			check.False(*alphaRequested, "discovery fell back to v1alpha1 after the v1beta1 request failed")
		})
	}
}
