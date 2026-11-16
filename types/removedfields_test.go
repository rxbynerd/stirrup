package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func decodeRunConfigStrict(data string) error {
	var cfg RunConfig
	dec := json.NewDecoder(bytes.NewReader([]byte(data)))
	dec.DisallowUnknownFields()
	return dec.Decode(&cfg)
}

func TestExplainRunConfigDecodeError_NamesImageAdmissionReplacement(t *testing.T) {
	must := require.New(t)
	decodeErr := decodeRunConfigStrict(`{"executor":{"type":"container","image":"ubuntu","registryAllowlist":["ghcr.io/acme/*"]}}`)
	must.Error(decodeErr)

	err := ExplainRunConfigDecodeError(decodeErr)
	must.ErrorIs(err, decodeErr)
	must.ErrorContains(err, "executor.registryAllowlist has been removed")
	must.ErrorContains(err, "--image-allowlist")
	must.ErrorContains(err, "STIRRUP_IMAGE_ALLOWLIST")
}

func TestExplainRunConfigDecodeError_LeavesOtherErrorsUnchanged(t *testing.T) {
	must := require.New(t)
	decodeErr := decodeRunConfigStrict(`{"executor":{"imagee":"ubuntu"}}`)
	must.Error(decodeErr)
	must.Equal(decodeErr, ExplainRunConfigDecodeError(decodeErr))

	other := errors.New("unexpected EOF")
	must.Equal(other, ExplainRunConfigDecodeError(other))
	must.NoError(ExplainRunConfigDecodeError(nil))
}
