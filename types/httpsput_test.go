package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const inlinePresignedURL = "https://bucket.example/workspace.tar.gz?X-Amz-Signature=deadbeefcafe"

func validateHTTPSPut(c *HTTPSPutConfig) []string {
	var errs []string
	validateHTTPSPutConfig("dest", c, &errs)
	return errs
}

func TestValidateHTTPSPutConfig_AcceptsSecretReferenceURLAndMixedHeaders(t *testing.T) {
	check := assert.New(t)
	check.Empty(validateHTTPSPut(&HTTPSPutConfig{
		URLRef: "secret://file:///var/run/stirrup/export-url",
		Headers: map[string]string{
			"x-ms-blob-type": "BlockBlob",
			"Content-Type":   "application/gzip",
			"Authorization":  "secret://EXPORT_AUTH",
			"X-Upload-Token": "secret://EXPORT_TOKEN",
		},
	}))
	check.Empty(validateHTTPSPut(nil))
}

func TestValidateHTTPSPutConfig_RejectsInlineURLWithoutEchoingIt(t *testing.T) {
	must := require.New(t)
	errs := validateHTTPSPut(&HTTPSPutConfig{URLRef: inlinePresignedURL})
	must.Len(errs, 1)
	must.Contains(errs[0], "dest.urlRef must be a secret reference")
	must.NotContains(errs[0], "deadbeefcafe")
	must.NotContains(errs[0], "bucket.example")
}

func TestValidateHTTPSPutConfig_RequiresURLRef(t *testing.T) {
	must := require.New(t)
	must.Equal([]string{"dest.urlRef is required"}, validateHTTPSPut(&HTTPSPutConfig{}))
}

func TestValidateHTTPSPutConfig_RejectsBadHeaders(t *testing.T) {
	tooMany := map[string]string{}
	for i := range MaxHTTPSPutHeaders + 1 {
		tooMany["x-h"+strings.Repeat("a", i)] = "v"
	}
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"name with a space", map[string]string{"bad name": "v"}, "invalid header name"},
		{"name with a colon", map[string]string{"x:y": "v"}, "invalid header name"},
		{"empty name", map[string]string{"": "v"}, "invalid header name"},
		{"client-owned header", map[string]string{"Content-Length": "5"}, `dest.headers["Content-Length"] is set by the HTTP client`},
		{"host header", map[string]string{"host": "other.example"}, `dest.headers["host"] is set by the HTTP client`},
		{"empty value", map[string]string{"x-a": ""}, `dest.headers["x-a"] must not be empty`},
		{"line break in value", map[string]string{"x-a": "v\r\nx-b: w"}, `dest.headers["x-a"] contains a control character`},
		{"oversized value", map[string]string{"x-a": strings.Repeat("v", MaxHTTPSPutHeaderValueBytes+1)}, `dest.headers["x-a"] must be at most`},
		{"names differing by case", map[string]string{"X-A": "v", "x-a": "w"}, "differ only by case"},
		{"too many", tooMany, "at most 32 are allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			errs := validateHTTPSPut(&HTTPSPutConfig{URLRef: "secret://U", Headers: tc.headers})
			must.NotEmpty(errs)
			must.Contains(strings.Join(errs, "\n"), tc.want)
		})
	}
}

func TestValidateHTTPSPutConfig_RequiresSecretReferenceForCredentialHeaders(t *testing.T) {
	for _, name := range []string{"Authorization", "cookie", "X-Api-Key", "x_api_key", "apikey", "Token", "X-Auth", "x-upload-auth", "Authentication", "x-amz-security-token", "x-amz-server-side-encryption-customer-key", "X-Upload-Signature", "Signature", "secret", "Password", "Key", "Credential", "Session"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			errs := validateHTTPSPut(&HTTPSPutConfig{URLRef: "secret://U", Headers: map[string]string{name: "plaintext-credential"}})
			must.Len(errs, 1)
			must.Contains(errs[0], "must be a secret reference")
			must.NotContains(errs[0], "plaintext-credential")
		})
	}
	for _, name := range []string{"x-ms-blob-type", "x-amz-server-side-encryption-aws-kms-key-id", "x-amz-server-side-encryption-customer-key-md5", "If-None-Match", "x-author", "x-tokenizer"} {
		t.Run(name, func(t *testing.T) {
			check := assert.New(t)
			check.False(HTTPSPutHeaderNeedsSecretRef(name))
		})
	}
}

func TestHTTPSPutConfig_SecretRefsListsURLThenHeaderReferences(t *testing.T) {
	check := assert.New(t)
	c := &HTTPSPutConfig{
		URLRef:  "secret://ssm:///export/url",
		Headers: map[string]string{"b": "secret://B", "a": "secret://A", "plain": "v"},
	}
	check.Equal([]string{"secret://ssm:///export/url", "secret://A", "secret://B"}, c.SecretRefs())
	check.Empty((*HTTPSPutConfig)(nil).SecretRefs())
	check.Empty((&HTTPSPutConfig{URLRef: inlinePresignedURL}).SecretRefs())
}

func TestRunConfigRedact_HidesHTTPSPutURLReferenceAndEveryHeaderValue(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := RunConfig{}
	cfg.Executor.WorkspaceExportHTTPSPut = &HTTPSPutConfig{
		URLRef:  "secret://EXPORT_URL",
		Headers: map[string]string{"Authorization": "secret://EXPORT_AUTH", "x-ms-blob-type": "BlockBlob"},
	}

	redacted := cfg.Redact()
	got := redacted.Executor.WorkspaceExportHTTPSPut
	must.NotNil(got)
	check.Equal("secret://[REDACTED]", got.URLRef)
	check.Equal(map[string]string{"Authorization": "secret://[REDACTED]", "x-ms-blob-type": "[REDACTED]"}, got.Headers)

	got.Headers["x-ms-blob-type"] = "mutated"
	check.Equal("BlockBlob", cfg.Executor.WorkspaceExportHTTPSPut.Headers["x-ms-blob-type"], "the redacted copy must not share the live header map")
	check.Equal("secret://EXPORT_URL", cfg.Executor.WorkspaceExportHTTPSPut.URLRef)
}

func TestRunConfigRedact_HidesAnInlineURLFromAnUnvalidatedConfig(t *testing.T) {
	must := require.New(t)
	cfg := RunConfig{}
	cfg.Executor.WorkspaceExportTo = inlinePresignedURL
	cfg.Executor.WorkspaceExportHTTPSPut = &HTTPSPutConfig{URLRef: inlinePresignedURL}

	raw, err := json.Marshal(cfg.Redact())
	must.NoError(err)
	must.NotContains(string(raw), "deadbeefcafe")
	must.NotContains(string(raw), "workspace.tar.gz")
}

func TestRunConfigRedact_KeepsAGCSWorkspaceExportDestination(t *testing.T) {
	must := require.New(t)
	cfg := RunConfig{}
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"
	must.Equal("gs://bucket/runs/r1/workspace.tar.gz", cfg.Redact().Executor.WorkspaceExportTo)
}

func executorExportErrors(cfg ExecutorConfig) []string {
	var errs []string
	validateExecutorWorkspaceExportTo(cfg, &errs)
	return errs
}

func TestValidateExecutorWorkspaceExport_AcceptsHTTPSPutOnArchivingExecutors(t *testing.T) {
	for _, typ := range []string{"local", "container", "k8s", "k8s-sandbox"} {
		t.Run(typ, func(t *testing.T) {
			check := assert.New(t)
			required := false
			check.Empty(executorExportErrors(ExecutorConfig{
				Type:                    typ,
				WorkspaceExportHTTPSPut: &HTTPSPutConfig{URLRef: "secret://EXPORT_URL"},
				WorkspaceExportRequired: &required,
			}))
		})
	}
}

func TestValidateExecutorWorkspaceExport_RejectsHTTPSPutMisuse(t *testing.T) {
	target := &HTTPSPutConfig{URLRef: "secret://EXPORT_URL"}
	cases := []struct {
		name string
		cfg  ExecutorConfig
		want string
	}{
		{"both destinations", ExecutorConfig{Type: "container", WorkspaceExportTo: "gs://b/o", WorkspaceExportHTTPSPut: target}, "mutually exclusive"},
		{"api executor", ExecutorConfig{Type: "api", WorkspaceExportHTTPSPut: target}, `executor.workspaceExportHttpsPut is not valid for executor.type="api"`},
		{"none executor", ExecutorConfig{Type: "none", WorkspaceExportHTTPSPut: target}, `executor.workspaceExportHttpsPut is not valid for executor.type="none"`},
		{"unset executor type", ExecutorConfig{WorkspaceExportHTTPSPut: target}, "executor.workspaceExportHttpsPut requires an explicit executor.type"},
		{"inline URL", ExecutorConfig{Type: "k8s", WorkspaceExportHTTPSPut: &HTTPSPutConfig{URLRef: inlinePresignedURL}}, "executor.workspaceExportHttpsPut.urlRef must be a secret reference"},
		{"empty block", ExecutorConfig{Type: "k8s", WorkspaceExportHTTPSPut: &HTTPSPutConfig{}}, "executor.workspaceExportHttpsPut.urlRef is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			errs := executorExportErrors(tc.cfg)
			must.NotEmpty(errs)
			must.Contains(strings.Join(errs, "\n"), tc.want)
		})
	}
}

func TestValidateExecutorWorkspaceExport_DoesNotEchoAnHTTPSURLPastedIntoWorkspaceExportTo(t *testing.T) {
	must := require.New(t)
	errs := executorExportErrors(ExecutorConfig{Type: "container", WorkspaceExportTo: inlinePresignedURL})
	must.Len(errs, 1)
	must.Contains(errs[0], "must use the gs:// scheme")
	must.Contains(errs[0], "executor.workspaceExportHttpsPut")
	must.NotContains(errs[0], "deadbeefcafe")
}

func TestExecutorConfig_ExportRequiredDefaultsToTrueForAnHTTPSPutDestination(t *testing.T) {
	check := assert.New(t)
	cfg := ExecutorConfig{WorkspaceExportHTTPSPut: &HTTPSPutConfig{URLRef: "secret://EXPORT_URL"}}
	check.True(cfg.ExportsWorkspace())
	check.True(cfg.ExportRequired())
	optional := false
	cfg.WorkspaceExportRequired = &optional
	check.False(cfg.ExportRequired())
	check.False(ExecutorConfig{}.ExportsWorkspace())
}

func resultSinkErrors(cfg *ResultSinkConfig) []string {
	var errs []string
	validateResultSinkConfig(cfg, &errs)
	return errs
}

func TestValidateResultSinkConfig_AcceptsAnHTTPSPutSink(t *testing.T) {
	check := assert.New(t)
	cfg := &ResultSinkConfig{
		Type:     "https-put",
		HTTPSPut: &HTTPSPutConfig{URLRef: "secret://RESULT_URL", Headers: map[string]string{"If-None-Match": "*"}},
	}
	check.Empty(resultSinkErrors(cfg))
	check.True(cfg.IsRemote())
	check.False(cfg.UsesGCPCredential())
}

func TestValidateResultSinkConfig_RejectsHTTPSPutMisuse(t *testing.T) {
	target := &HTTPSPutConfig{URLRef: "secret://RESULT_URL"}
	cases := []struct {
		name string
		cfg  *ResultSinkConfig
		want string
	}{
		{"missing block", &ResultSinkConfig{Type: "https-put"}, `resultSink type "https-put" requires httpsPut`},
		{"inline URL", &ResultSinkConfig{Type: "https-put", HTTPSPut: &HTTPSPutConfig{URLRef: inlinePresignedURL}}, "resultSink.httpsPut.urlRef must be a secret reference"},
		{"block on gcs", &ResultSinkConfig{Type: "gcs", Bucket: "stirrup-results", HTTPSPut: target}, `resultSink.httpsPut is only valid when type is "https-put"`},
		{"block on stdout-json", &ResultSinkConfig{Type: "stdout-json", HTTPSPut: target}, `resultSink.httpsPut is only valid when type is "https-put"`},
		{"bucket on https-put", &ResultSinkConfig{Type: "https-put", HTTPSPut: target, Bucket: "stirrup-results"}, `resultSink.bucket is only valid when type is "gcs"`},
		{"credential on https-put", &ResultSinkConfig{Type: "https-put", HTTPSPut: target, Credential: &CredentialConfig{Type: "gcp-default"}}, "resultSink.credential is only valid when type is"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			errs := resultSinkErrors(tc.cfg)
			must.Len(errs, 1)
			must.Contains(errs[0], tc.want)
			must.NotContains(errs[0], "deadbeefcafe")
		})
	}
}

func TestResultSinkConfig_IsRemoteAndUsesGCPCredentialByType(t *testing.T) {
	cases := map[string][2]bool{
		"none":        {false, false},
		"stdout-json": {false, false},
		"gcs":         {true, true},
		"gcp-pubsub":  {true, true},
		"https-put":   {true, false},
	}
	for typ, want := range cases {
		t.Run(typ, func(t *testing.T) {
			check := assert.New(t)
			cfg := &ResultSinkConfig{Type: typ}
			check.Equal(want[0], cfg.IsRemote(), "IsRemote")
			check.Equal(want[1], cfg.UsesGCPCredential(), "UsesGCPCredential")
		})
	}
	check := assert.New(t)
	check.False((*ResultSinkConfig)(nil).IsRemote())
	check.False((*ResultSinkConfig)(nil).UsesGCPCredential())
}

func TestRunConfigRedact_HidesTheHTTPSPutResultSinkDestination(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := RunConfig{ResultSink: &ResultSinkConfig{
		Type:     "https-put",
		HTTPSPut: &HTTPSPutConfig{URLRef: "secret://RESULT_URL", Headers: map[string]string{"Authorization": "secret://RESULT_AUTH"}},
	}}

	got := cfg.Redact().ResultSink
	must.NotNil(got)
	must.NotNil(got.HTTPSPut)
	check.Equal("secret://[REDACTED]", got.HTTPSPut.URLRef)
	check.Equal(map[string]string{"Authorization": "secret://[REDACTED]"}, got.HTTPSPut.Headers)
	check.Equal("secret://RESULT_URL", cfg.ResultSink.HTTPSPut.URLRef, "the live config is untouched")
}
