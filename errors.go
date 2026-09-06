package camfarm

import "errors"

// The error taxonomy. These are kept distinct because conflating them is a known
// pain for consumers of virtual cameras: a caller that cannot tell "this build
// cannot do that" from "this is not ready yet" has to guess.
var (
	// ErrUnknownCamera means no camera with the given ID exists in the fleet.
	ErrUnknownCamera = errors.New("camfarm: unknown camera")

	// ErrUnsupported means this build cannot do what was asked. It is not a
	// malfunction and not a transient condition.
	ErrUnsupported = errors.New("camfarm: unsupported")

	// ErrNotReady means the fleet or camera has not finished initialising.
	ErrNotReady = errors.New("camfarm: not ready")

	// ErrFaultActive means the request was refused *because* a fault is
	// deliberately in effect.
	//
	// This must never read as a malfunction. "This camera is lying on purpose"
	// and "this camera is broken" being indistinguishable would make the whole
	// tool useless.
	ErrFaultActive = errors.New("camfarm: refused by an active fault")
)
