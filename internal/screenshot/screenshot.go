// Package screenshot grabs one decoded frame from a live RTSP stream.
//
// Capture dials the stream like any RTSP client, waits for the next random
// access point, decodes it, and returns it. It knows nothing about camfarm
// fleets: it takes a URL, so it works against any RTSP source and exercises
// the same wire path a real consumer would.
package screenshot

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync"

	"github.com/Eyevinn/hi264/pkg/decoder"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
)

// ErrNoSupportedVideo means no H.264 video was found in the stream's SDP.
// H.265 is not yet supported, so an H.265-only stream also yields this error.
var ErrNoSupportedVideo = errors.New("screenshot: no H.264 video found (H.265 is not yet supported)")

// Capture returns the next decodable frame of the stream at rtspURL. It blocks
// until a random access point arrives, the stream ends, or ctx is done; ctx's
// own error is returned unwrapped so errors.Is(err, context.DeadlineExceeded)
// works for callers.
func Capture(ctx context.Context, rtspURL string) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := base.ParseURL(rtspURL)
	if err != nil {
		return nil, fmt.Errorf("screenshot: parsing %q: %w", rtspURL, err)
	}

	c := &gortsplib.Client{
		Scheme:            u.Scheme,
		Host:              u.Host,
		OnTransportSwitch: func(error) {},
	}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("screenshot: connecting to %s: %w", u.Host, err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, c.Close)
	defer stop()

	desc, _, err := c.Describe(u)
	if err != nil {
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: DESCRIBE: %w", err))
	}

	medi, forma, grab, err := newGrabber(desc)
	if err != nil {
		return nil, err
	}
	if err := c.SetupAll(desc.BaseURL, []*description.Media{medi}); err != nil {
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: SETUP: %w", err))
	}

	type result struct {
		img image.Image
		err error
	}
	done := make(chan result, 1)
	var once sync.Once
	finish := func(img image.Image, err error) {
		once.Do(func() { done <- result{img, err} })
	}

	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		img, err := grab(pkt)
		if err != nil {
			finish(nil, err)
		} else if img != nil {
			finish(img, nil)
		}
	})
	if _, err := c.Play(nil); err != nil {
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: PLAY: %w", err))
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Wait() }()

	select {
	case r := <-done:
		return r.img, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-closed:
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: stream ended before a frame arrived: %w", err))
	}
}

// ctxOr prefers the context's error when the context is what caused err.
func ctxOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// grabber consumes RTP packets and returns a non-nil image once one decodes.
type grabber func(*rtp.Packet) (image.Image, error)

func newGrabber(desc *description.Session) (*description.Media, format.Format, grabber, error) {
	var f264 *format.H264
	if medi := desc.FindFormat(&f264); medi != nil {
		g, err := newH264Grabber(f264)
		return medi, f264, g, err
	}
	return nil, nil, nil, ErrNoSupportedVideo
}

// An RTP depacketising error is not fatal: mid-fragment joins and loss are
// normal on a live stream and simply mean "keep waiting". Only a failed decode
// of a random access point is reported, since that is the frame being asked for.
func newH264Grabber(f *format.H264) (grabber, error) {
	rd, err := f.CreateDecoder()
	if err != nil {
		return nil, fmt.Errorf("screenshot: H.264 RTP decoder: %w", err)
	}
	dec := decoder.New()
	return func(pkt *rtp.Packet) (image.Image, error) {
		nalus, err := rd.Decode(pkt)
		if err != nil {
			return nil, nil
		}
		if !h264.IsRandomAccess(nalus) {
			return nil, nil
		}
		au := withParamSets(nalus, f.SPS, f.PPS)
		fr, err := dec.DecodeNALUs(au)
		if err != nil {
			return nil, fmt.Errorf("screenshot: decoding H.264 random access point: %w", err)
		}
		return h264Image(fr), nil
	}, nil
}

// withParamSets prepends the SDP's parameter sets. The RTP stream need not
// repeat them in-band, and the decoders cannot start without them. Empty sets
// (H.264 has no VPS) are skipped.
func withParamSets(nalus [][]byte, sets ...[]byte) [][]byte {
	out := make([][]byte, 0, len(nalus)+len(sets))
	for _, s := range sets {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return append(out, nalus...)
}
