package screenshot

import (
	"image"

	hi264frame "github.com/Eyevinn/hi264/pkg/frame"
	"github.com/Eyevinn/hi264/pkg/yuv"
)

// h264Image converts a decoded H.264 frame to RGB, honouring the colour
// metadata hi264 carries from the SPS.
func h264Image(f *hi264frame.Frame) image.Image {
	return yuv.FrameToImage(f)
}
