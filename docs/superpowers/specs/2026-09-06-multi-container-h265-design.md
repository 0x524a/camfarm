# Multi-container ingestion and H.265 passthrough

Status: approved design, not yet implemented.
Supersedes nothing. Extends `2026-09-05-camfarm-architecture-design.md`, which assumed a
single container (MPEG-TS) and a single codec (H.264).

## 0. What this slice adds

Two capabilities, chosen together because they intersect rather than multiply:

1. **Containers.** Accept plain MP4, fragmented MP4, and MPEG-TS as file sources, detected
   from content rather than filename.
2. **Codecs.** Serve H.265 as well as H.264, by **passthrough only**: a source's codec is
   the codec that camfarm serves. No transcoding.

The intersection is free. Both containers carry both codecs, and one parse layer covers the
2×3 matrix without a combinatorial explosion of parsers (§3).

### 0.1 What it deliberately does not add

- **No transcoding.** H.264 in cannot become H.265 out. §2.4 records why this is a hard
  constraint rather than a scheduling choice.
- **No rescaling.** A 1080p source cannot be served as a 320×240 camera. Same reason:
  rescaling requires re-encoding. Deferred to a later slice (§9).
- **No new fault effects.** The fault catalogue stays refused, exactly as it is today.
- **No Matroska, no MPEG-PS, no Annex-B elementary streams.** §12 records why each was
  excluded and what would change the answer.

## 1. Motivation

The concrete trigger: a real 5-minute 1080p H.264 file (`liberty_bell_5min_Uniform.mp4`,
MP4 with an AAC track) could not be used as a source without a manual
`ffmpeg -c:v copy -an -f mpegts` remux first. Requiring every consumer to pre-remux their
test media is friction that defeats the point of a drop-in test fixture, and the remux step
is a place for a developer to introduce a discrepancy between "the file I have" and "the
file camfarm served".

MP4 is the format a screen recorder, a phone, a downloaded clip, and `ffmpeg -c copy` all
produce. It is the default shape of "I have a test clip". Accepting it directly is the
single highest-value-per-line change available to the media path.

H.265 is a separate motivation: a video-analytics pipeline that decodes H.264 correctly and
H.265 incorrectly is a real and common class of bug, and camfarm cannot currently produce
the input that exposes it.

## 2. Evidence base

Claims here are split by how they were established. This project's own rule is that no
claim goes into a commit message or a README that was not produced by the thing asserting
it, and the same discipline applies to a design doc.

### 2.1 Verified directly against source in the module cache

Read at `$(go env GOMODCACHE)/github.com/bluenviron/mediacommon/v2@v2.9.4/`:

- `pkg/formats/` contains exactly four packages: `mpegts`, `mp4`, `fmp4`, `pmp4`.
- `pkg/formats/pmp4/presentation.go:35` — `func (p *Presentation) Unmarshal(r io.ReadSeeker) error`.
  **Plain, non-fragmented MP4 demuxing already exists in a dependency camfarm already has.**
- `pkg/formats/pmp4/track.go:31` — `Track{ID int, TimeScale uint32, TimeOffset int32, Codec codecs.Codec, Samples []*Sample}`.
- `pkg/formats/pmp4/sample.go:4` — `Sample{Duration uint32, PTSOffset int32, IsNonSyncSample bool, PayloadSize uint32, GetPayload func() ([]byte, error)}`.
  Payload retrieval is lazy, via a closure.
- `pkg/formats/fmp4/sample.go:4` — `Sample{Duration uint32, PTSOffset int32, IsNonSyncSample bool, Payload []byte}`.
  Payload is eager here, an asymmetry with `pmp4` that the container parsers must absorb.
- `pkg/formats/fmp4/sample.go:112` — `func (ps Sample) GetH265() ([][]byte, error)`, which
  delegates to `GetH264()` at `:119`. A **value** receiver, not a pointer receiver. The
  delegation is correct rather than lazy: AVCC length-prefix framing is codec-agnostic.
- `pkg/formats/mp4/codecs/h265.go` — `H265{VPS, SPS, PPS []byte}`.
- `internal/mp4/codec_boxes.go:62` — `h265FindParams(hvcc *amp4.HvcC)`; `:252` — `case "hvcC"`;
  `:268` — `r.Codec = &codecs.H265{...}`. **hvcC is parsed on the read path**, so H.265 in
  MP4 works, not only H.264.
- `pkg/codecs/h264/avcc.go:27` — the NALU length is read as a hardcoded four-byte
  big-endian integer.
- `LengthSizeMinusOne` appears only at `internal/mp4/codec_boxes.go:759` and `:821`, both on
  the **write** path, both hardcoded to `3`. It is never read back when parsing. The
  four-byte assumption above is therefore **not checkable through the library's API**
  (§13.1).
- `pkg/codecs/` includes both `h264` and `h265`.

### 2.2 Established by delegated research, not independently re-verified

Reported by a subagent that read the source and cited file paths. Spot-checked where the
design depends on it; recorded here as second-hand so that a later reader knows which
claims to re-check before relying on them:

- `pkg/codecs/h265/sps.go` exposes `SPS.Unmarshal`, `Width()`, `Height()`, `FPS()` — the same
  three methods `internal/media/mpegts.go:93-95` already calls on `h264.SPS`. `FPS()` returns
  0 when VUI timing is absent, matching camfarm's existing "zero means undeclared" convention.
- `pkg/codecs/h265/is_random_access.go` exposes `IsRandomAccess(au [][]byte) bool`, true for
  `IDR_W_RADL`, `IDR_N_LP`, and `CRA_NUT`.
- **`pkg/codecs/h265/` contains no VPS parser.** Only SPS and PPS. VPS can be captured as an
  opaque NALU and passed through, which is all passthrough serving needs (§13.3).
- `pkg/formats/mpegts/reader.go:267` — `OnDataH265`, with a callback signature identical to
  `OnDataH264`.
- `gortsplib/v5@v5.6.5/pkg/format/h265.go` — `format.H265{PayloadTyp uint8, VPS, SPS, PPS []byte, MaxDONDiff int}`.
  No `PacketizationMode` field; RFC 7798 has no equivalent of RFC 6184's packetization mode.
- `rtph265.Encoder.Encode(au [][]byte) ([]*rtp.Packet, error)` — identical signature to the
  `rtph264` encoder already used at `internal/rtsp/pump.go:146`.
- `rtph265.Encoder.Init()` returns an error when `MaxDONDiff != 0` ("not supported (yet)").
- `abema/go-mp4` v1.7.1 (MIT) is the box-parsing engine under `pmp4`/`fmp4`, and is already
  in the module graph transitively.

### 2.3 Established empirically by delegated research

A subagent generated H.265 fixtures locally with `ffmpeg`/`libx265` and inspected them:

- At settings mirroring the existing H.264 fixture (320×240, 2s, `-crf 30`, `-g 15`, `-bf 0`),
  the H.265 output was **52,076 bytes against the H.264 fixture's 40,232**. H.265 is *larger*
  at this size: per-keyframe VPS+SPS+PPS+AUD+SEI overhead dominates any coding-efficiency gain
  over two seconds of trivial content.
- **libx265 defaults to open GOP.** The first keyframe was `IDR_N_LP` (type 20); the second was
  `CRA_NUT` (type 21), not an IDR. This is the trap in §13.2.
- VPS/SPS/PPS recur in-band before every keyframe, so camfarm's existing "require in-band
  parameter sets, refuse if absent" rule ports directly — with VPS added to the requirement.
- libx265 also emits an access-unit delimiter (`AUD_NUT`, type 35) per frame, which H.264
  baseline typically does not. Harmless to the RTP encoder, but an H.265 access unit's NALU
  list is one element longer per frame than the H.264 equivalent. Any test asserting exact
  NALU counts must account for it.

### 2.4 Why transcoding is out of scope, not merely deferred

Transcoding requires decoding to raw pixels and re-encoding. Nothing in `go.mod` contains a
video decoder or an H.265 encoder — `mediacommon` and `gortsplib` only ever parse and
depacketize bitstreams that already exist. No viable pure-Go H.265 encoder exists as a
dependency today. Achieving it would require cgo bindings to libx265/libavcodec, or invoking
an `ffmpeg` binary at runtime. Both are excluded by hard constraints: `CGO_ENABLED=0`, and
ffmpeg is a test-and-fixture-generation dependency only.

Consequence, which must be stated wherever the feature is documented: **"serve H.265" means
"bring an H.265 source."** It is not a capability the farm can conjure from an H.264 fixture.
The same reasoning governs rescaling.

### 2.5 Explicitly unverified

- Whether `mediamtx` itself uses `pmp4`/`fmp4` internally. Plausible from shared authorship;
  the research on it failed to confirm. Not cited as support for anything here.
- Whether a production-grade pure-Go Matroska or MPEG-PS demuxer exists. The survey returned
  degraded results, so §12's exclusion of those formats rests on "none was found", **not** on
  "none exists". If either format becomes a requirement, that survey must be redone before
  "hand-roll it" is accepted as a conclusion.
- Whether `libx265` is present in the GitHub Actions `ubuntu-latest` runner image. §10 makes
  this question irrelevant to CI rather than answering it.

## 3. Decision — parse layer shape

Three shapes were weighed for the 2-codec × 3-container matrix.

**Rejected: container-shaped, codec branch inside.** Each container parser branches on codec
internally. Least abstraction and closest to today's file, but NAL classification, SPS
parsing, and random-access determination get written once per container. Every trap found in
§2.3 lives in codec knowledge, so duplicating it is duplicating the traps.

**Rejected: full normalization to a common intermediate.** Every container converts to
Annex-B plus 90 kHz timestamps plus sync flags, then a single codec-aware finalizer runs.
Best testability, but MP4's already-split NALUs get needlessly reframed and an extra
allocation pass is added to load.

**Chosen: codec-shaped seam, with the intermediate's testability kept.** Container parsers are
codec-dumb and emit NALU lists plus timing. One adapter per codec owns all codec knowledge.
The adapter's interface accepts plain NALU lists and never sees a container, so it is fully
testable without any container present — the property that made normalization attractive,
obtained without the reframing cost.

Five units (three containers, two codecs) replace six near-duplicate parsers, and each trap
in §13 has exactly one place to be got wrong.

## 4. The codec adapter contract

```go
// paramSets holds a codec's out-of-band parameter sets. VPS is H.265-only and
// stays nil for H.264.
type paramSets struct{ VPS, SPS, PPS []byte }

// codecAdapter owns everything codec-specific about classifying NAL units.
//
// It takes plain NALU lists and never sees a container, so every adapter is
// testable against hand-built input with no fixture and no demuxer.
type codecAdapter interface {
	// Codec identifies what this adapter handles.
	Codec() Codec
	// Classify records any parameter sets found in au into ps.
	Classify(au [][]byte, ps *paramSets)
	// IsRandomAccess reports whether a decoder can begin at au.
	IsRandomAccess(au [][]byte) bool
	// Validate reports whether ps holds every parameter set this codec needs.
	Validate(ps paramSets) error
	// Geometry derives the advertised dimensions and frame rate from ps.
	Geometry(ps paramSets) (width, height int, fps float64, err error)
}
```

| | `h264Adapter` | `h265Adapter` |
|---|---|---|
| NAL type | `h264.NALUType(n[0] & 0x1F)` | `h265.NALUType((n[0] >> 1) & 0b111111)` |
| Header size | 1 byte | 2 bytes |
| Required sets | SPS, PPS | VPS, SPS, PPS |
| Random access | `h264.IsRandomAccess` | `h265.IsRandomAccess` |
| Geometry | `h264.SPS` | `h265.SPS` |

Neither adapter hand-rolls a random-access check. §13.2 explains why that rule is absolute.

## 5. Container parsers

Containers emit bytes and timing, and know nothing about codecs beyond which track to pick:

```go
// rawSample is one sample as a container reports it, before any codec-specific
// validation or rebasing.
type rawSample struct {
	NALUs [][]byte
	// PTS and DTS are in 90 kHz units, not yet rebased so the first sample is 0.
	PTS, DTS int64
	// Sync is the container's own random-access declaration. SyncKnown reports
	// whether the container declared anything at all: MPEG-TS does not.
	Sync      bool
	SyncKnown bool
}
```

### 5.1 MPEG-TS

Refactor of today's `parseMPEGTS`. The read loop, the separate PTS/DTS time decoders, the
`OnDecodeError` capture, and `copyNALUs` all stay. What changes: track selection accepts
`CodecH264` or `CodecH265` and registers `OnDataH264` or `OnDataH265` accordingly, and the
NALU classification that currently sits inline moves into the adapter. `SyncKnown` is always
false — MPEG-TS carries no sync-sample table.

### 5.2 Plain MP4

`pmp4.Presentation.Unmarshal(io.ReadSeeker)`, then select the first track whose `Codec` is
`codecs.H264` or `codecs.H265`. Parameter sets come from that `Codec` value, which
`internal/mp4/codec_boxes.go` has already extracted from avcC or hvcC. Each sample's
`GetPayload()` returns AVCC-framed bytes, split by `h264.AVCC.Unmarshal` (correct for both
codecs — the framing is codec-agnostic, per §2.1).

### 5.3 Fragmented MP4

`fmp4.Init.Unmarshal` for parameter sets, then `fmp4.Parts.Unmarshal` for samples. NALUs come
pre-split from `Sample.GetH264()`/`GetH265()`, so no AVCC step is needed. Note the receiver
is a value, not a pointer, and that `Sample.Payload` is eager here where `pmp4`'s is a
closure.

### 5.4 Timestamps

MP4 hands over a per-track `TimeScale` plus per-sample `Duration` and `PTSOffset` rather than
absolute timestamps. Reconstruction: accumulate DTS by summing `Duration`; compute
`PTS = DTS + PTSOffset`; rescale both by `value * 90000 / TimeScale` in `int64`, multiplying
before dividing so precision is not lost. `Track.TimeOffset` (the edit-list initial delay)
needs no separate handling, because camfarm already rebases the first access unit to zero and
the offset drops out.

### 5.5 Decision — the codec adapter owns RandomAccess

Two candidate authorities exist for whether a sample is a random-access point: the
container's own table (`stss`, surfaced as `Sample.IsNonSyncSample`) and the bitstream's NALU
types.

**Decided: the codec adapter is authoritative. Where a container also declares a value, the
two are cross-checked and a disagreement is a load error naming both.**

Reasoning. Whether a client can actually begin decoding at a given access unit is a property
of the bitstream, not of the muxer's bookkeeping, and routing every container through one
code path keeps MPEG-TS and MP4 consistent instead of subtly divergent. A file whose `stss`
disagrees with its own bitstream is exactly the sort of input that would quietly undermine a
determinism claim, and this project's stated preference is to fail loudly.

Accepted risk: this is stricter than the specification requires and could reject a legal
file. Mitigation is that it fails loudly with both values named, so the cause is immediate
rather than mysterious. **First implementation task after the parsers work is to run it
against the real 5-minute 1080p MP4 in §1.** If real-world files trip it, the fallback is to
demote the mismatch from an error to a recorded observation, and that reversal must be a
deliberate, documented decision rather than a quiet loosening.

## 6. Sniffing

```go
func sniff(header []byte) (container, error)
```

| Container | Signature |
|---|---|
| ISOBMFF (MP4/MOV, plain or fragmented) | `header[4:8] == "ftyp"` |
| MPEG-TS | `0x47` at offsets 0, 188, and 376 |

Plain versus fragmented is **not** pre-sniffed. `pmp4.Presentation.Unmarshal` is attempted
first and `fmp4` is the fallback, because distinguishing them properly requires inspecting
`moov` for an `mvex` box, and trying-then-falling-back is both shorter and more robust than a
heuristic.

Detection is **content-only. Filename extensions are never consulted.** An extension can
lie, and a `.mp4` that actually holds MPEG-TS bytes should load rather than fail. An
unrecognised file produces an error quoting the first eight bytes in hex and naming every
supported container, so a user who brings a Matroska file learns why it was refused instead
of seeing a parse failure from the wrong parser.

Checking three MPEG-TS sync bytes at the 188-byte stride rather than one at offset 0 avoids
misidentifying a file that merely happens to begin with `0x47`.

## 7. Media model

```go
type Media struct {
	Codec  Codec
	VPS    []byte // H.265 only; nil for H.264
	SPS    []byte
	PPS    []byte
	Width  int
	Height int
	FPS    float64
	AUs    []AccessUnit
}
```

One added field. **No discriminated union and no interface**, deliberately: every consumer
that needs codec-specific behaviour already branches on `Media.Codec`, so introducing
struct-level polymorphism would duplicate a dispatch that already exists at the call sites
that matter, and would force a type switch into `pump.go` for no behavioural gain. A nil
check is sufficient, and matches the existing convention that `FPS == 0` means "the
bitstream declared nothing".

The struct remains read-only once `Load` returns, and remains shared across every camera
using the same source. An always-nil slice field costs nothing at that granularity.

## 8. RTSP serving path

- `internal/rtsp/server.go` — `camera.forma` widens from `*format.H264` to the
  `format.Format` interface, which both concrete types implement. The format construction
  branches on `Media.Codec` to build `format.H264{PayloadTyp, SPS, PPS, PacketizationMode}`
  or `format.H265{PayloadTyp, VPS, SPS, PPS}`. Note the H.265 literal has **no**
  `PacketizationMode` — mirroring the H.264 literal and deleting the field is the natural
  mistake, and it is a compile error rather than a silent one.
- The parameter-set presence guard gains a VPS requirement when the codec is H.265.
- `internal/rtsp/pump.go` — the `enc` field becomes
  `interface{ Encode([][]byte) ([]*rtp.Packet, error) }`, which both encoders already satisfy
  structurally. `NewPump` branches on codec to construct one or the other. `MaxDONDiff` is
  left at its zero value; a non-zero value makes `Init()` fail, and zero is correct for a
  single-layer synthetic source anyway.
- **`Pump.Step()` needs no change.** This is the most important line in this section. The
  fault-decision seam, the RTP timestamp arithmetic, the loop handling, and the sequence-number
  derivation all live in `Step()`, and every determinism guarantee this project makes rests
  there. A feature that adds a codec without touching that function cannot regress those
  guarantees.
- `spec.go` — the literal `c.Video.Codec != "H264"` rejection is relaxed to accept `"H265"`,
  which resolves the TODO already standing at `spec.go:145-147`.

## 9. VideoSpec redefinition

`VideoSpec`'s fields are **redefined to mean desired output**, not asserted source
properties. A value equal to the source's own passes; a differing value returns
`ErrUnsupported` whose message names the behaviour that will eventually exist.

### 9.1 Accepted risk, stated plainly

This carries a real cost, chosen with the tradeoff understood rather than overlooked. A spec
that is **refused today will silently begin rescaling** on the version where rescaling lands.
The refusal becomes an action, with no compile error and no signal at the call site.

The alternative considered was a separate `Scale` block, leaving `Width`/`Height` as the
assertion they are now, so that the later slice only removes a refusal and no meaning ever
changes. That was rejected in favour of the smaller API surface.

Mitigations, all of which are obligations on the implementation:

- Every refusal message must name the future behaviour explicitly, so a developer reading it
  learns that the field will later act rather than refuse.
- The behaviour change must be called out in the release notes of the version that lands
  rescaling, not merely in a changelog line.
- The refusal must be `ErrUnsupported`, so a caller can distinguish "not yet" from "invalid".

## 10. Fixtures and CI

**Two fixtures are committed: MPEG-TS/H.264 (40,232 bytes, already present) and
MPEG-TS/H.265 (~52 KB, new). MP4 and fMP4 variants are generated at test time with
`ffmpeg -c copy`.**

This is a remux, not an encode. Consequences, in order of importance:

1. **The unverified `libx265`-in-CI question stops mattering.** CI needs only plain `ffmpeg`,
   which the workflow already installs. `libx265` is needed solely by a maintainer
   regenerating the committed H.265 fixture. Rather than asserting a claim that was
   established by analogy (§2.5), the design removes the claim from the critical path.
2. The MP4 tests exercise genuinely ffmpeg-produced files, which is what users will bring,
   instead of files produced by the same library under test.
3. Only two fixtures are committed rather than six, so the repository does not accumulate a
   near-duplicate binary per container.

The H.265 fixture is generated with `-bf 0`, mirroring the existing H.264 fixture's
deliberate choice. That keeps PTS equal to DTS, so this change does not simultaneously
introduce a second codec *and* the first B-frame reordering the pump has ever seen. B-frame
reordering is worth testing, as its own change with its own reasoning.

## 11. Testing

### 11.1 Cross-container equivalence is the primary invariant

One source, remuxed to MPEG-TS, plain MP4, and fMP4, must yield an identical access-unit
count, byte-identical NALU payloads, and identical rebased PTS and DTS.

This single test is the load-bearing one. The three most probable bugs in this slice are
timescale rescaling errors (§5.4), AVCC splitting errors (§5.2), and off-by-one accumulation
of sample durations. None is reliably caught by a per-container test, because a per-container
test asserts against numbers derived the same wrong way. Comparing two independent paths to
the same source catches all three.

### 11.2 Remaining coverage

- **Adapter table tests** with hand-built NALU lists, no container and no fixture. This is
  the payoff of §3's chosen shape, and the natural home for a regression test asserting that
  a `CRA_NUT` access unit is treated as random-access.
- **Sniffing** table tests, including a `.mp4`-named file holding MPEG-TS bytes, a truncated
  header, and an unrecognised container whose error names the supported set.
- **External verification** by `ffprobe` against a served H.265 stream, extending the existing
  practice rather than trusting camfarm's own parse.
- **Determinism** — the existing seed-to-sequence-number tests extended to an H.265 camera,
  confirming §8's claim that an untouched `Step()` preserves them.
- **Mixed fleet** — H.264 and H.265 cameras in one fleet, different containers, confirming
  that per-camera codec dispatch is genuinely per camera.
- **The real file** — the 5-minute 1080p MP4 from §1, as the check on §5.5's strictness.

## 12. Deferred formats

- **Annex-B elementary streams (`.h264`, `.265`).** A parser already exists in mediacommon, so
  the cost is near zero. Excluded anyway because the format carries no dimensions, no
  sync-sample table, and no timing: frame rate and geometry would have to be supplied by the
  caller, which makes this a configuration-surface decision rather than a parsing one. It also
  has no reliable magic bytes, since H.264 and H.265 share the start-code convention. Worth
  adding once there is a reason to decide where that metadata comes from.
- **Matroska / WebM.** No mediacommon support. The libraries known to exist are EBML structure
  parsers rather than full demuxers with codec-box semantics, so this is building a demuxer,
  not calling one. MKV is also common as an archived download but rare as something a camera or
  a streaming pipeline *produces*, which is a poor fit for a synthetic camera farm. Note §2.5:
  the library survey behind this was degraded and would need redoing.
- **MPEG-PS.** Same shape of judgment. Real but narrowing use (DVR and NVR exports), no
  mediacommon support, no library found. Hand-rolling a container parser is precisely the
  "large set of approximate behaviours" this project's constraints reject.

## 13. Accepted risks

### 13.1 The AVCC four-byte length assumption

`h264.AVCC.Unmarshal` reads a hardcoded four-byte length prefix (§2.1) and mediacommon never
surfaces `LengthSizeMinusOne` on the read path, so the assumption cannot be validated through
the API. An MP4 written with a non-default length size would misparse. Encoders in practice
emit `LengthSizeMinusOne = 3`, and a mismatch produces `invalid length` in almost every case
rather than silent corruption. Accepted, recorded, not worked around.

### 13.2 Open-GOP H.265 keyframes are not IDRs

libx265 defaults to open GOP, so keyframes after the first are `CRA_NUT`, not IDR (§2.3).
A hand-rolled random-access check that tests only for IDR types finds the IDR at the start of
a file, looks correct under casual testing, and then treats every subsequent keyframe as a
non-random-access point — surfacing at the loop boundary, far from its cause.

Therefore: **random-access determination goes through `h265.IsRandomAccess` and
`h264.IsRandomAccess`, never through a local NALU-type comparison.** §11.2 requires a
regression test pinning this.

### 13.3 No VPS parser exists

mediacommon parses H.265 SPS and PPS but not VPS (§2.2). Opaque passthrough needs nothing
more. The gap becomes real the moment anyone wants VPS-derived metadata or a VPS-level fault
such as a deliberate VPS/SPS mismatch, and at that point a parser has to be written or
contributed upstream. Recorded so that a later reader does not assume symmetry with SPS.

### 13.4 fMP4 rejects some legal files

`fmp4.Parts.Unmarshal` requires `trunFlagDataOffsetPreset` in every `trun` and errors with
`unsupported flags` otherwise, so a producer using the base-data-offset convention is
refused. This fails loudly, which is the right failure mode, but someone will eventually
bring a file that does not load and the cause will not be obvious from the message. Worth a
line in the README's scope section.

### 13.5 Strictness of the sync cross-check

§5.5's disagreement-is-an-error rule may reject legal files. Validated against a real file
before the slice is considered done; reversal, if needed, is a documented decision.

## 14. Public API delta

- `SourceSpec.Kind` is unchanged. `file` now means "any supported container, detected from
  content". No `mp4` or `fmp4` kind is added: a caller should not have to know, and telling
  camfarm the wrong one should not be possible.
- `VideoSpec.Codec` accepts `"H265"` in addition to `"H264"`.
- `VideoSpec` field semantics are redefined per §9.
- `Media` gains `VPS` (internal; not part of the public surface).
- `ErrUnsupported` is reused for every refusal. No new exported error.

Nothing already public breaks in this slice. The one forward-looking hazard is §9.1's, and it
lands on a later version rather than this one.
