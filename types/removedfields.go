package types

import (
	"fmt"
	"strings"
)

// removedRunConfigFields maps the JSON key of a field RunConfig no longer
// has to the message naming its replacement.
var removedRunConfigFields = map[string]string{
	"registryAllowlist": "executor.registryAllowlist has been removed; the deployment sets the sandbox image admission policy with `stirrup harness --image-allowlist` or the STIRRUP_IMAGE_ALLOWLIST environment variable (docs/security.md#image-admission-policy)",
}

// ExplainRunConfigDecodeError appends the replacement to a strict JSON
// decoder's unknown-field error when the field is one RunConfig has removed,
// and returns any other error unchanged.
func ExplainRunConfigDecodeError(err error) error {
	if err == nil {
		return nil
	}
	for key, replacement := range removedRunConfigFields {
		if strings.Contains(err.Error(), fmt.Sprintf("unknown field %q", key)) {
			return fmt.Errorf("%w: %s", err, replacement)
		}
	}
	return err
}
