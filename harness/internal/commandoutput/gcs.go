package commandoutput

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/gcs"
)

type GCSUploaderOptions struct {
	Bucket          string
	ObjectPrefix    string
	Bearer          credential.BearerTokenFunc
	HTTPClient      *http.Client
	EndpointBaseURL string
}

type GCSUploader struct{ opts GCSUploaderOptions }

func NewGCSUploader(opts GCSUploaderOptions) (*GCSUploader, error) {
	if opts.Bucket == "" || opts.Bearer == nil {
		return nil, fmt.Errorf("command output GCS uploader requires bucket and bearer token")
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return &GCSUploader{opts: opts}, nil
}

func (u *GCSUploader) UploadCommandOutputArchive(ctx context.Context, localPath, archiveID string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open archive for upload: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat archive for upload: %w", err)
	}
	object := archiveID + ".command-output.tar.gz"
	if u.opts.ObjectPrefix != "" {
		object = strings.TrimSuffix(u.opts.ObjectPrefix, "/") + "/" + object
	}
	if err := gcs.UploadObject(ctx, u.opts.HTTPClient, gcs.UploadOptions{
		Bucket: u.opts.Bucket, Object: object, BodyReader: f, BodySize: info.Size(), ContentType: "application/gzip",
		Bearer: u.opts.Bearer, EndpointBaseURL: u.opts.EndpointBaseURL,
	}); err != nil {
		return "", err
	}
	return "gs://" + u.opts.Bucket + "/" + object, nil
}
