# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this
repository.

## Project name

`camfarm` is settled, kept deliberately rather than by default. Availability and collision were
checked on 2026-09-06: no module on `pkg.go.dev` matches, and the one exact-name GitHub repository
is an unrelated project in another field, which cannot conflict in any case because Go module paths
are namespaced by owner. The name must not embed the ONVIF word mark, and does not.

## What this project is

`camfarm` is a synthetic RTSP and ONVIF camera farm, written in Go, for testing video-analytics
pipelines. It serves N fake cameras with controllable resolution, codec, bitrate, and deliberate
misbehaviour, and it answers ONVIF control-plane requests so that camera-discovery code can find and
drive the fake fleet.

## The problem it solves

Anyone building video analytics needs many cameras to test against. The common improvisation is
`mediamtx` plus ffmpeg loops, which produces streams but neither fault injection nor an ONVIF
control plane, and that is where a large share of real bugs live. Cameras in the field lie about
their capabilities, drop frames, starve keyframes, present odd auth challenges, and tear down
mid-session. A test rig that cannot reproduce those conditions cannot catch the bugs they cause.

## Determinism is the product

A fault the farm injects must be reproducible from a seed, or it is useless for characterizing a
bug. Every source of randomness, every timing decision, every choice of which frame to drop or which
byte to corrupt must trace back to a seed that a test can log and a developer can replay. This
applies from the first fault-injection code written, not as a retrofit. If a design choice would
make a fault's outcome depend on wall-clock timing, goroutine scheduling, or unseeded randomness,
that choice needs to be revisited before it ships.

## Local sibling repositories

These repositories are checked out locally on the same account and are directly relevant prior art.
Read before designing.

- `/home/ritwik/devBed/rj/onvif-go/`, module `github.com/0x524a/onvif-go` (MIT). Read `server/`
  carefully before making any architectural decision here: it already implements a working virtual
  multi-lens ONVIF camera covering the Device, Media, PTZ, and Imaging SOAP services, with
  WS-Security digest authentication and in-memory PTZ/imaging state. It does not include an actual
  RTSP server: `GetStreamURI` returns a string URI and its own documentation says video streaming
  needs an external integration such as `mediamtx`, ffmpeg, or a custom implementation. It has no
  fault injection and no WS-Discovery responder (the `discovery/` package at the top level of that
  repo is a probe-sending client, not something that would make a fake device discoverable). Its
  `testdata/` holds XML captures from real cameras, useful as a reference for what realistic-looking
  ONVIF responses look like, but it is aimed at client compatibility testing, not at producing RTSP
  media fixtures.
- `/home/ritwik/devBed/rj/rtspeek/`, module `github.com/0x524A/rtspeek` (note the capital `A`; Go
  module paths are case-sensitive). This is RTSP OPTIONS/DESCRIBE inspection, codec classification,
  and SPS parsing. It matters here in two ways: it is a plausible *client* to point at this farm,
  and its test suite is directly useful prior art for serving RTSP in Go, since it stands up real
  `gortsplib` servers in its own tests rather than mocking them.
- Forks of `gortsplib` (the RTSP client/server library this account already uses) and of
  `rtsp-simple-server`/`mediamtx` exist on the same account and are familiar territory.

Two sibling projects are being scaffolded in parallel. Reference them conceptually but do not assume
their APIs, since they may still be in flux:

- `/home/ritwik/devBed/rj/framelag/`: a glass-to-glass video latency profiler. `camfarm` is the
  controllable source it measures against in CI. Together they form two halves of one test rig for
  video pipelines: `camfarm` produces controllable, faulty streams; `framelag` measures what happens
  to them end to end.
- `/home/ritwik/devBed/rj/onvif-mcp/`: an MCP server exposing ONVIF camera control to LLM agents.
  `camfarm` is a candidate consumer for that project's integration tests, not its current test
  strategy: as of this writing its own design doc and plan make no reference to `camfarm` and rely on
  `onvif-go/server` directly plus hand-authored `httptest` fixtures.

## Open architectural decisions

None of the following has been decided. Frame each honestly with evidence before committing to an
answer; do not invent a decision that has not been made.

1. **The central build decision.** Depend on `onvif-go`'s `server/` package as an import, extract
   and generalize the relevant parts into this repository, or reimplement from scratch. Depending keeps
   one source of truth for the ONVIF control-plane logic and lets improvements flow both ways, but
   couples the two repositories and constrains this project to whatever shape that package already has.
   Extracting risks two copies of the same ONVIF server drifting apart over time. Reimplementing
   duplicates working code for no clear benefit. A fourth option: contribute the generalization
   upstream to `onvif-go` itself, with this project becoming a thin fleet orchestrator over it.
   Everything else about the ONVIF side of this project follows from how this is resolved, so resolve
   it first.

2. **Scope: RTSP only, or RTSP plus the ONVIF control plane.** The ONVIF half is the entire
   differentiator against `mediamtx` plus ffmpeg loops, and it is also most of the remaining work.
   Decide explicitly whether the first usable version includes it or defers it.

3. **Where the video comes from.** Candidates: live-generated synthetic test patterns (needs an
   encoder in the loop), pre-encoded looping fixture files (cheap, portable, but limited variety), or
   passthrough of a user-supplied file. Encoder options if generation is live: pure Go (very limited
   H.264/H.265 support today), cgo bindings to ffmpeg (capable, but costs portability and CI
   simplicity), or shipping a small set of pre-encoded fixtures instead of encoding at all. This
   decision determines whether the project can stay `CGO_ENABLED=0`, which in turn determines how
   easily it runs in CI and in a scratch container.

4. **The fault-injection catalogue.** This is the actual value this project adds and deserves the
   most design attention of anything here. Candidates worth weighing: frame drop at a set rate, jitter
   and burst delay, mid-stream bitrate and resolution change, keyframe starvation, SDP that
   misdescribes the actual codec, a `DESCRIBE` response advertising capabilities the stream does not
   then deliver, Digest versus Basic auth quirks and wrong-realm handling, incorrect `Content-Base`,
   teardown mid-session, TCP reset, RTSP interleaved-versus-UDP transport differences, and timestamp
   discontinuities. Decide which belong in the first usable version and which are later work, and hold
   each candidate to the standard that it maps to a real class of field bug rather than being arbitrary
   chaos for its own sake.

5. **Determinism and reproducibility mechanics.** How a seed is threaded through fault selection,
   how a failing test reports the seed and fleet spec needed to reproduce the failure, and what "replay
   this exact run" looks like as a developer-facing workflow.

6. **Scale model.** Streams-per-process versus a process per camera, the resource ceiling of each
   approach, and roughly how many cameras a developer laptop can serve under each. The answer here
   should be measured, not asserted; build the harness to make that measurement rather than guessing at
   a number up front.

7. **Discovery.** Whether to implement a WS-Discovery responder so real ONVIF clients can find the
   fake fleet via multicast, and if so, how to handle the practical failure modes: multicast inside
   Docker, multicast in CI runners, and multicast across network namespaces. This is a common source of
   "works on a laptop, fails in CI," so the design needs to account for it rather than discover it
   later.

8. **Configuration surface.** A declarative fleet spec (for example YAML) describing the fleet up
   front, versus a runtime API for adding, removing, and mutating cameras while a test is running,
   versus offering both. Note that both `framelag` and `onvif-mcp` will want to drive this farm
   programmatically in the middle of a test run, which bears on how much a static declarative spec
   alone can cover.

9. **Library or binary.** This needs to be usable as a Go library from another project's tests,
   something like `camfarm.Start(spec)` inside a `TestMain`, and not only as a standalone binary,
   because making other repositories testable without hardware is the whole point of building it. Frame
   what that implies for the public API surface: what must be stable, what can change, and what a
   caller needs from a single import.

## Hard constraints

- **Personal project framing only.** This is not, and must never be described as, work done at, for,
  or informed by any employer or client. Do not name any employer or client anywhere in code,
  comments, commit messages, documentation, or issues. This is a firm rule with no exceptions.
- **It simulates; it is not a conformance-tested ONVIF implementation, and it must never claim to
  be.** The README must carry an explicit "Scope / what this does not do" section stating plainly
  what is not faithfully emulated. Overclaiming here is actively harmful: someone could test their
  client against this farm and wrongly conclude they are ONVIF-compliant.
- **License: MIT**, matching `onvif-go`.
- **Default branch: `main`.**
- **CI green from the first commit.** No badge that is not backed by a workflow currently passing.
  No coverage percentage stated anywhere that CI itself did not produce. This project has less
  excuse than most to get this wrong, since it is itself a test tool and should be exemplary about
  testing its own claims.
- Prefer a small set of faithfully-simulated behaviours over a large set of approximate ones. When
  in doubt about whether a feature belongs in an early version, favor the smaller, correct set.

## Quality bar

Every fault-injection behaviour and every ONVIF response this project produces should be something a
maintainer can point to a real device's behaviour and say "this is what that looked like." Vague,
unmotivated chaos is not the goal; reproducible, explainable misbehaviour is. Code that cannot be
tested deterministically (a fixed seed producing a fixed sequence of events) does not belong in the
fault-injection path.

## Relationship to framelag and onvif-mcp

`camfarm` is the controllable, faulty source; `framelag` is the measurement instrument that watches
what a pipeline does when it receives that source; `onvif-mcp` is a consumer of the ONVIF control
plane that needs something real, but not physical, to talk to during its own tests. None of the
three should assume internal details of the others beyond whatever public API each settles on.

