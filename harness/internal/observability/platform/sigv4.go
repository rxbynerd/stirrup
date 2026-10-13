package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
)

// sigV4Transport signs each request with AWS Signature Version 4 for one
// service. Every attempt is signed afresh because signatures are
// time-bound and the exporter's retries re-enter the transport.
type sigV4Transport struct {
	base        http.RoundTripper
	credentials aws.CredentialsProvider
	signer      *v4.Signer
	service     string
	region      string
	now         func() time.Time
}

func (t *sigV4Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read request body for signing: %w", err)
		}
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	signed := req.Clone(req.Context())
	signed.Body = io.NopCloser(bytes.NewReader(body))
	signed.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	signed.ContentLength = int64(len(body))
	signed.Header.Set("X-Amz-Content-Sha256", payloadHash)

	creds, err := t.credentials.Retrieve(req.Context())
	if err != nil {
		return nil, credentialsError(err)
	}
	if err := t.signer.SignHTTP(req.Context(), creds, signed, payloadHash, t.service, t.region, t.now()); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}
	return t.base.RoundTrip(signed)
}

// credentialsError reports a credential retrieval failure. A
// credential_process error is replaced with a fixed message because the
// SDK quotes the process output, which can hold the secret key, and export
// errors reach stderr unscrubbed.
func credentialsError(err error) error {
	var processErr *processcreds.ProviderError
	if errors.As(err, &processErr) {
		return errors.New("retrieve AWS credentials: credential_process failed (its output is withheld because it can contain credentials)")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("retrieve AWS credentials: the default credential chain found none within %s (set AWS_PROFILE or AWS_ACCESS_KEY_ID, or run under an instance or pod role): %w", credentialCheckTimeout, err)
	}
	return fmt.Errorf("retrieve AWS credentials: %w", err)
}
