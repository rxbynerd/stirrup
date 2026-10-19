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
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://host.example/path?api_key=supersecret",
			Err: cause,
		}
		// The url.Error message embeds the query secret, which is what the unwrap must remove.
		require.Contains(t, wrapped.Error(), "supersecret", "test premise: url.Error must embed the query secret")

		got := UnwrapURLError(wrapped)
		require.Same(t, cause, got)
		require.NotContains(t, got.Error(), "supersecret")
	})

	t.Run("returns non-url errors unchanged", func(t *testing.T) {
		plain := errors.New("some other failure")
		require.Same(t, plain, UnwrapURLError(plain))
	})

	t.Run("finds a url error nested deeper in the chain", func(t *testing.T) {
		wrapped := &url.Error{Op: "Post", URL: "https://host?token=abc", Err: cause}
		chain := errors.Join(errors.New("context"), wrapped)
		require.Same(t, cause, UnwrapURLError(chain))
	})

	t.Run("nil error is returned unchanged", func(t *testing.T) {
		require.NoError(t, UnwrapURLError(nil))
	})
}
