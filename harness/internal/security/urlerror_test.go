package security

import (
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnwrapURLError_ReturnsTransportCause(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")

	t.Run("unwraps url error to transport cause", func(t *testing.T) {
		must := require.New(t)
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://host.example/path?api_key=supersecret",
			Err: cause,
		}
		// The url.Error message embeds the query secret, which is what the unwrap must remove.
		must.Contains(wrapped.Error(), "supersecret", "test premise: url.Error must embed the query secret")

		got := UnwrapURLError(wrapped)
		must.Same(cause, got)
		must.NotContains(got.Error(), "supersecret")
	})

	t.Run("returns non-url errors unchanged", func(t *testing.T) {
		must := require.New(t)
		plain := errors.New("some other failure")
		must.Same(plain, UnwrapURLError(plain))
	})

	t.Run("finds a url error nested deeper in the chain", func(t *testing.T) {
		must := require.New(t)
		wrapped := &url.Error{Op: "Post", URL: "https://host?token=abc", Err: cause}
		chain := errors.Join(errors.New("context"), wrapped)
		must.Same(cause, UnwrapURLError(chain))
	})

	t.Run("nil error is returned unchanged", func(t *testing.T) {
		must := require.New(t)
		must.NoError(UnwrapURLError(nil))
	})
}
