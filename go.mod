// Module path is settled: the name was checked for collision and kept (see
// docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md §11.2).
// The name must not embed the ONVIF word mark, and does not.
module github.com/0x524a/camfarm

go 1.26.0

require (
	github.com/0x524a/onvif-go v1.2.0
	github.com/Eyevinn/hi264 v0.10.0
	github.com/bluenviron/gortsplib/v5 v5.6.6
	github.com/bluenviron/mediacommon/v2 v2.9.5
	github.com/pion/rtp v1.10.5
	// Used only by the e2e-tagged dashboard tests; not part of the shipped binary.
	github.com/playwright-community/playwright-go v0.6000.0
)

require (
	github.com/0x524A/rtspeek v1.0.0
	github.com/pion/rtcp v1.2.17
)

require (
	github.com/Eyevinn/mp4ff v0.50.0 // indirect
	github.com/abema/go-mp4 v1.7.3 // indirect
	github.com/asticode/go-astikit v0.30.0 // indirect
	github.com/asticode/go-astits v1.16.0 // indirect
	github.com/deckarep/golang-set/v2 v2.8.0 // indirect
	github.com/go-jose/go-jose/v3 v3.0.5 // indirect
	github.com/go-stack/stack v1.8.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/sdp/v3 v3.0.20 // indirect
	github.com/pion/srtp/v3 v3.0.15 // indirect
	github.com/pion/transport/v4 v4.1.0 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)
