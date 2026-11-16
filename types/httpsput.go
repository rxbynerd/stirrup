package types

import (
	"fmt"
	"sort"
	"strings"
)

// HTTPSPutConfig names a destination the harness uploads one object to
// with a single HTTP PUT: a presigned object-store URL (S3, R2, GCS,
// Azure Blob) or an endpoint the control plane serves itself. The URL is
// a capability, so it never appears in RunConfig: URLRef is a secret
// reference resolved each time the harness uploads. See
// docs/configuration.md#https-put-destinations.
type HTTPSPutConfig struct {
	// URLRef is a "secret://" reference resolving to the destination URL.
	// The resolved URL must use https://, or http:// with a loopback host.
	URLRef string `json:"urlRef"`

	// Headers are sent on the PUT request. Values may be plaintext or a
	// "secret://" reference. A header that conventionally carries a
	// credential (see HTTPSPutHeaderNeedsSecretRef) must be a reference.
	// A Content-Type entry replaces the harness's own. A resolved value
	// is held to MaxHTTPSPutHeaderValueBytes.
	Headers map[string]string `json:"headers,omitempty"`
}

const (
	// MaxHTTPSPutHeaders caps HTTPSPutConfig.Headers.
	MaxHTTPSPutHeaders = 32
	// MaxHTTPSPutHeaderNameBytes caps one header name.
	MaxHTTPSPutHeaderNameBytes = 256
	// MaxHTTPSPutHeaderValueBytes caps one plaintext header value or
	// secret reference.
	MaxHTTPSPutHeaderValueBytes = 8192
)

// httpsPutTransportHeaders are owned by the HTTP client; a configured
// value would desynchronise the request from its body or its connection.
var httpsPutTransportHeaders = map[string]bool{
	"host":                true,
	"content-length":      true,
	"transfer-encoding":   true,
	"connection":          true,
	"upgrade":             true,
	"te":                  true,
	"trailer":             true,
	"expect":              true,
	"keep-alive":          true,
	"proxy-connection":    true,
	"proxy-authorization": true,
}

var httpsPutCredentialHeaderNames = map[string]bool{
	"authorization":  true,
	"authentication": true,
	"cookie":         true,
	"token":          true,
	"apikey":         true,
	"api-key":        true,
}

var httpsPutCredentialHeaderSuffixes = []string{
	"-token", "-key", "-apikey", "-auth", "-secret", "-signature", "-password", "-credential",
}

// HTTPSPutHeaderNeedsSecretRef reports whether a header name
// conventionally carries a credential, so its value must be a
// "secret://" reference rather than plaintext in RunConfig. Underscores
// are read as hyphens.
func HTTPSPutHeaderNeedsSecretRef(name string) bool {
	lower := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	if httpsPutCredentialHeaderNames[lower] {
		return true
	}
	for _, suffix := range httpsPutCredentialHeaderSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// SecretRefs returns the secret references c carries, URL first. Nil-safe.
func (c *HTTPSPutConfig) SecretRefs() []string {
	if c == nil {
		return nil
	}
	var refs []string
	if strings.HasPrefix(c.URLRef, "secret://") {
		refs = append(refs, c.URLRef)
	}
	names := make([]string, 0, len(c.Headers))
	for name := range c.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v := c.Headers[name]; strings.HasPrefix(v, "secret://") {
			refs = append(refs, v)
		}
	}
	return refs
}

// redacted returns a deep copy of c with the URL reference and every
// header value replaced by a placeholder. Header names are kept.
func (c *HTTPSPutConfig) redacted() *HTTPSPutConfig {
	if c == nil {
		return nil
	}
	out := &HTTPSPutConfig{}
	if c.URLRef != "" {
		out.URLRef = "secret://[REDACTED]"
	}
	if len(c.Headers) > 0 {
		out.Headers = make(map[string]string, len(c.Headers))
		for name, v := range c.Headers {
			if strings.HasPrefix(v, "secret://") {
				out.Headers[name] = "secret://[REDACTED]"
			} else {
				out.Headers[name] = "[REDACTED]"
			}
		}
	}
	return out
}

// validateHTTPSPutConfig checks the shape of an HTTPS PUT destination at
// path. Messages name fields and header names but never echo a value: a
// URL pasted inline in place of a reference is a capability.
func validateHTTPSPutConfig(path string, c *HTTPSPutConfig, errs *[]string) {
	if c == nil {
		return
	}
	switch {
	case c.URLRef == "":
		*errs = append(*errs, fmt.Sprintf("%s.urlRef is required", path))
	case !strings.HasPrefix(c.URLRef, "secret://"):
		*errs = append(*errs, fmt.Sprintf(
			"%s.urlRef must be a secret reference (e.g. \"secret://NAME\") that resolves to the URL; an inline URL is not accepted because the URL is a capability", path))
	case len(c.URLRef) > MaxHTTPSPutHeaderValueBytes:
		*errs = append(*errs, fmt.Sprintf("%s.urlRef must be at most %d bytes", path, MaxHTTPSPutHeaderValueBytes))
	}

	if len(c.Headers) > MaxHTTPSPutHeaders {
		*errs = append(*errs, fmt.Sprintf("%s.headers has %d entries; at most %d are allowed", path, len(c.Headers), MaxHTTPSPutHeaders))
	}
	names := make([]string, 0, len(c.Headers))
	for name := range c.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]string{}
	for _, name := range names {
		value := c.Headers[name]
		if !validHTTPHeaderName(name) || len(name) > MaxHTTPSPutHeaderNameBytes {
			*errs = append(*errs, fmt.Sprintf(
				"%s.headers has an invalid header name %s (an HTTP token of at most %d bytes is required)",
				path, truncateForError([]byte(name)), MaxHTTPSPutHeaderNameBytes))
			continue
		}
		lower := strings.ToLower(name)
		if prev, dup := seen[lower]; dup {
			*errs = append(*errs, fmt.Sprintf("%s.headers names %q and %q differ only by case", path, prev, name))
			continue
		}
		seen[lower] = name
		if httpsPutTransportHeaders[lower] {
			*errs = append(*errs, fmt.Sprintf("%s.headers[%q] is set by the HTTP client and cannot be configured", path, name))
			continue
		}
		switch {
		case value == "":
			*errs = append(*errs, fmt.Sprintf("%s.headers[%q] must not be empty", path, name))
		case len(value) > MaxHTTPSPutHeaderValueBytes:
			*errs = append(*errs, fmt.Sprintf("%s.headers[%q] must be at most %d bytes", path, name, MaxHTTPSPutHeaderValueBytes))
		case !ValidHTTPSPutHeaderValue(value):
			*errs = append(*errs, fmt.Sprintf("%s.headers[%q] contains a control character", path, name))
		case HTTPSPutHeaderNeedsSecretRef(name) && !strings.HasPrefix(value, "secret://"):
			*errs = append(*errs, fmt.Sprintf(
				"%s.headers[%q] must be a secret reference (e.g. \"secret://NAME\"); this header carries a credential", path, name))
		}
	}
}

// validHTTPHeaderName reports whether name is an RFC 9110 token.
func validHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// ValidHTTPSPutHeaderValue reports whether value is free of the bytes
// that could split or truncate a header line: every control character
// except horizontal tab.
func ValidHTTPSPutHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
