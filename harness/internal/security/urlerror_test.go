package security

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestUnwrapURLError(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")

	t.Run("unwraps url.Error to transport cause", func(t *testing.T) {
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://host.example/path?api_key=supersecret",
			Err: cause,
		}
		// Sanity: the *url.Error message would itself leak the query secret.
		if !strings.Contains(wrapped.Error(), "supersecret") {
			t.Fatalf("test premise broken: url.Error did not embed the query secret: %q", wrapped.Error())
		}
		got := UnwrapURLError(wrapped)
		if got != cause {
			t.Fatalf("UnwrapURLError returned %v, want the transport cause", got)
		}
		if strings.Contains(got.Error(), "supersecret") {
			t.Errorf("unwrapped error still leaks the query secret: %q", got.Error())
		}
	})

	t.Run("returns non-url errors unchanged", func(t *testing.T) {
		plain := errors.New("some other failure")
		if got := UnwrapURLError(plain); got != plain {
			t.Fatalf("UnwrapURLError(%v) = %v, want it unchanged", plain, got)
		}
	})

	t.Run("finds a url.Error nested deeper in the chain", func(t *testing.T) {
		wrapped := &url.Error{Op: "Post", URL: "https://host?token=abc", Err: cause}
		chain := errors.Join(errors.New("context"), wrapped)
		if got := UnwrapURLError(chain); got != cause {
			t.Fatalf("UnwrapURLError on a joined chain = %v, want the transport cause", got)
		}
	})

	t.Run("nil error is returned unchanged", func(t *testing.T) {
		if got := UnwrapURLError(nil); got != nil {
			t.Fatalf("UnwrapURLError(nil) = %v, want nil", got)
		}
	})
}
