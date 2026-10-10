package core

import "errors"

// ErrCancelledByControlPlane is the cause attached to the run's context when
// a "cancel" ControlEvent arrives from the control plane, distinguishing it
// via context.Cause from deadline expiry or caller-initiated cancellation.
var ErrCancelledByControlPlane = errors.New("cancelled by control plane")

// ErrControlStreamLost is the cause attached to the run's context when the
// control stream ends while the run is active; see
// AgenticLoop.ControlStreamLost.
var ErrControlStreamLost = errors.New("control stream lost")
