package core

import (
	"errors"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// executorStartsImage reports whether the executor type starts its sandbox
// from executor.image.
func executorStartsImage(executorType string) bool {
	return executorType == "container" || types.IsK8sFamily(executorType)
}

// admitExecutorImage applies the deployment's image admission policy to
// executor.image. An empty image is left to the executor builder, which
// reports it as missing.
func admitExecutorImage(cfg types.ExecutorConfig, policy executor.ImageAdmissionPolicy, sec *security.SecurityLogger) error {
	if !executorStartsImage(cfg.Type) || cfg.Image == "" {
		return nil
	}
	if err := policy.Admit(cfg.Image); err != nil {
		if errors.Is(err, executor.ErrImageNotAdmitted) {
			sec.ImageAdmissionDenied(cfg.Type, cfg.Image, policy.Patterns())
		} else {
			sec.ConfigValidationFailed([]string{err.Error()})
		}
		return fmt.Errorf("executor.image: %w", err)
	}
	if !policy.Configured() && !executor.ImageDigestPinned(cfg.Image) {
		sec.ImageUnpinnedWithoutAdmissionPolicy(cfg.Type, cfg.Image)
	}
	return nil
}
