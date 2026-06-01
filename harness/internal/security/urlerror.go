package security

import (
	"errors"
	"net/url"
)

// UnwrapURLError returns the transport-level cause of a *url.Error, or err
// unchanged when err is not a *url.Error.
//
// http.Client.Do wraps every transport failure in a *url.Error whose Error()
// string embeds the full request URL. Go redacts the userinfo component of
// that URL (user:***) but does NOT redact the query string, so a request to a
// credentialed URL (https://host/path?api_key=secret) leaks the secret
// whenever the *url.Error is %w-wrapped into a returned or logged error
// (CWE-532). Callers that interpolate a Do error into a message a credentialed
// URL could reach must unwrap it here first.
func UnwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
