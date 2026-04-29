package core

import "errors"

// ErrCancelledByControlPlane is the cause attached to the run's context when
// a "cancel" ControlEvent arrives from the control plane. The outer Run loop
// inspects context.Cause to distinguish control-plane cancellation from
// deadline expiry or caller-initiated cancellation, and records the outcome
// accordingly ("cancelled" vs "timeout" vs "error"). Tests can match it with
// errors.Is.
var ErrCancelledByControlPlane = errors.New("cancelled by control plane")
