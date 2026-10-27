// Package thumb makes small JPEG thumbnails of pictures.
package thumb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"io"

	// Picture formats Make can read.
	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// MaxSide is the longest side of a thumbnail, the same as Telegram's.
const MaxSide = 320

// MaxPixels is the largest picture Make decodes, so a huge image can't use
// up memory (a 12000 x 8000 photo is 96 million pixels).
const MaxPixels = 100_000_000

// Make returns a JPEG thumbnail of the picture in data, at most MaxSide
// pixels on its longest side and turned upright using its EXIF orientation.
func Make(data []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxPixels {
		return nil, errors.New("picture too large for a thumbnail")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > MaxSide || h > MaxSide {
		if w >= h {
			w, h = MaxSide, max(1, h*MaxSide/b.Dx())
		} else {
			w, h = max(1, w*MaxSide/b.Dy()), MaxSide
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	out := upright(dst, orientation(data))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// upright applies EXIF orientation o (1 to 8) to m.
func upright(m *image.RGBA, o int) image.Image {
	if o < 2 || o > 8 {
		return m
	}
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 { // rotated by 90 degrees one way or the other
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			out.SetRGBA(nx, ny, m.RGBAAt(x, y))
		}
	}
	return out
}

// orientation reads the EXIF orientation of a JPEG, or 1 when there is none.
func orientation(data []byte) int {
	r := bytes.NewReader(data)
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil || soi != [2]byte{0xFF, 0xD8} {
		return 1
	}
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr[0] != 0xFF {
			return 1
		}
		marker, n := hdr[1], int(binary.BigEndian.Uint16(hdr[2:]))-2
		if marker == 0xDA || n < 0 { // the picture data starts; no EXIF before it
			return 1
		}
		seg := make([]byte, n)
		if _, err := io.ReadFull(r, seg); err != nil {
			return 1
		}
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return exifOrientation(seg[6:])
		}
	}
}

// exifOrientation finds tag 0x0112 in the first directory of a TIFF block.
func exifOrientation(t []byte) int {
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	count := int(bo.Uint16(t[off:]))
	for i := 0; i < count; i++ {
		e := off + 2 + i*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if o := int(bo.Uint16(t[e+8:])); o >= 1 && o <= 8 {
				return o
			}
			return 1
		}
	}
	return 1
}
