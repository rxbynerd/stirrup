package core

import "errors"

// ErrCancelledByControlPlane is the cause attached to the run's context when
// a "cancel" ControlEvent arrives from the control plane, distinguishing it
// via context.Cause from deadline expiry or caller-initiated cancellation.
var ErrCancelledByControlPlane = errors.New("cancelled by control plane")
