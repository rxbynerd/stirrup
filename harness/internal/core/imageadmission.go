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

// imageVerdict is the outcome of judging executor.image against the
// deployment's image admission policy.
type imageVerdict int

const (
	// imageNotJudged: the executor does not start from executor.image, or
	// the image is empty and left to the executor builder to report.
	imageNotJudged imageVerdict = iota
	// imageDenied: a valid reference the configured policy refuses.
	imageDenied
	// imageMalformed: not a valid image reference.
	imageMalformed
	// imageAdmitted: the configured policy admits the reference.
	imageAdmitted
	// imagePinnedUnconfigured: no policy is configured and the reference is
	// pinned by digest.
	imagePinnedUnconfigured
	// imageUnpinnedUnconfigured: no policy is configured and the reference
	// can move to different content.
	imageUnpinnedUnconfigured
)

// judgeExecutorImage is the one place that decides what the policy means for
// executor.image; the run build and the dry run both map its verdict. The
// error is non-nil exactly for imageDenied and imageMalformed.
func judgeExecutorImage(cfg types.ExecutorConfig, policy executor.ImageAdmissionPolicy) (imageVerdict, error) {
	if !executorStartsImage(cfg.Type) || cfg.Image == "" {
		return imageNotJudged, nil
	}
	if err := policy.Admit(cfg.Image); err != nil {
		if errors.Is(err, executor.ErrImageNotAdmitted) {
			return imageDenied, err
		}
		return imageMalformed, err
	}
	switch {
	case policy.Configured():
		return imageAdmitted, nil
	case executor.ImageDigestPinned(cfg.Image):
		return imagePinnedUnconfigured, nil
	default:
		return imageUnpinnedUnconfigured, nil
	}
}

// admitExecutorImage applies the deployment's image admission policy to
// executor.image, records the verdict in the security log, and classifies a
// refusal for the control plane: a refused image is a sandbox_image failure
// and a malformed reference is config_invalid.
func admitExecutorImage(cfg types.ExecutorConfig, policy executor.ImageAdmissionPolicy, sec *security.SecurityLogger) error {
	verdict, err := judgeExecutorImage(cfg, policy)
	switch verdict {
	case imageDenied:
		sec.ImageAdmissionDenied(cfg.Type, cfg.Image, policy.Patterns())
		return markBuildFailure(types.BuildFailureSandboxImage, fmt.Errorf("executor.image: %w", err))
	case imageMalformed:
		sec.ConfigValidationFailed([]string{err.Error()})
		return markBuildFailure(types.BuildFailureConfigInvalid, fmt.Errorf("executor.image: %w", err))
	case imageUnpinnedUnconfigured:
		sec.ImageUnpinnedWithoutAdmissionPolicy(cfg.Type, cfg.Image)
	}
	return nil
}
