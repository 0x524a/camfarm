// Package camfarm serves synthetic RTSP video streams and answers the
// control-plane requests that camera-discovery code uses to find and drive a
// fleet, so that video-analytics pipelines can be tested without physical
// hardware.
//
// Its distinguishing property is deterministic fault injection: a fault fires
// because a seed said it would, at a reproducible point in the stream, and the
// same seed and fleet spec reproduce it exactly.
//
// This version serves RTSP media for a fleet of cameras and carries the seed and
// fault-injection seams throughout, with every catalogued fault deliberately
// inert: a fault named in a spec is refused rather than silently ignored. The
// control plane and the fault effects follow. The architecture is recorded in
// docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md.
package camfarm
