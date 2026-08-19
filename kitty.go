package tiletea

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
)

// encodeKittyGraphics encodes an RGBA image as a Kitty graphics protocol
// escape sequence spanning the given number of terminal columns and rows.
func encodeKittyGraphics(img *image.RGBA, cols, rows int) string {
	encoded := base64.StdEncoding.EncodeToString(img.Pix)

	const chunkSize = 4096
	var b bytes.Buffer

	for i := 0; i < len(encoded); i += chunkSize {
		end := min(i+chunkSize, len(encoded))
		chunk := encoded[i:end]

		m := 1
		if end == len(encoded) {
			m = 0
		}

		if i == 0 {
			fmt.Fprintf(&b, "\033_Ga=T,f=32,s=%d,v=%d,c=%d,r=%d,C=1,z=-1,m=%d;%s\033\\",
				img.Bounds().Dx(), img.Bounds().Dy(), cols, rows, m, chunk)
		} else {
			fmt.Fprintf(&b, "\033_Gm=%d;%s\033\\", m, chunk)
		}
	}
	return b.String()
}
