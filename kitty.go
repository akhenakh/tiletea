package tiletea

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
)

// encodeKittyGraphics encodes an RGBA image as a Kitty graphics protocol
// escape sequence spanning the given number of terminal columns and rows, at
// the given z-index.
//
// The image is PNG-compressed (f=100). Transmitting raw pixels (f=32) sends
// width*height*4 bytes per frame — tens of MB for a full-screen retina image
// — which the terminal must parse before displaying anything, causing
// multi-second display lag despite fast renders.
func encodeKittyGraphics(img *image.RGBA, cols, rows, zIndex int) string {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	const chunkSize = 4096
	var b bytes.Buffer

	// Delete any previous placements before transmitting the new image, so a
	// re-render (e.g. after a resize) replaces the old image instead of
	// leaving it behind on screen.
	b.WriteString("\033_Ga=d\033\\")

	for i := 0; i < len(encoded); i += chunkSize {
		end := min(i+chunkSize, len(encoded))
		chunk := encoded[i:end]

		m := 1
		if end == len(encoded) {
			m = 0
		}

		if i == 0 {
			fmt.Fprintf(&b, "\033_Ga=T,f=100,s=%d,v=%d,c=%d,r=%d,C=1,z=%d,m=%d;%s\033\\",
				img.Bounds().Dx(), img.Bounds().Dy(), cols, rows, zIndex, m, chunk)
		} else {
			fmt.Fprintf(&b, "\033_Gm=%d;%s\033\\", m, chunk)
		}
	}
	return b.String()
}
