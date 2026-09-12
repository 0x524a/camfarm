// Module path is settled: the name was checked for collision and kept (see
// docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md §11.2).
// The name must not embed the ONVIF word mark, and does not.
module github.com/0x524a/camfarm

go 1.26.0

require (
	github.com/bluenviron/gortsplib/v5 v5.6.5
	github.com/bluenviron/mediacommon/v2 v2.9.4
	github.com/pion/rtp v1.10.5
)

require (
	github.com/abema/go-mp4 v1.7.1 // indirect
	github.com/asticode/go-astikit v0.30.0 // indirect
	github.com/asticode/go-astits v1.16.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.17 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.13 // indirect
	github.com/pion/transport/v4 v4.1.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/0x524a/onvif-go => /home/ritwik/devBed/rj/onvif-go
