package thumb

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

// picture is w x h, red on the left half and blue on the right.
func picture(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 0, 0, 255}
			if x >= w/2 {
				c = color.RGBA{0, 0, 255, 255}
			}
			m.SetRGBA(x, y, c)
		}
	}
	return m
}

func size(t *testing.T, data []byte) (int, int, image.Image) {
	t.Helper()
	m, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("thumbnail isn't a JPEG: %v", err)
	}
	return m.Bounds().Dx(), m.Bounds().Dy(), m
}

func TestEachFormatShrinks(t *testing.T) {
	src := picture(1200, 800)
	var p, j, g bytes.Buffer
	png.Encode(&p, src)
	jpeg.Encode(&j, src, nil)
	gif.Encode(&g, src, nil)
	for name, data := range map[string][]byte{"png": p.Bytes(), "jpeg": j.Bytes(), "gif": g.Bytes()} {
		out, err := Make(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if w, h, _ := size(t, out); w != 320 || h != 213 {
			t.Errorf("%s: got %dx%d, want 320x213", name, w, h)
		}
	}
}

func TestSmallPictureKeepsItsSize(t *testing.T) {
	var p bytes.Buffer
	png.Encode(&p, picture(100, 60))
	out, err := Make(p.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := size(t, out); w != 100 || h != 60 {
		t.Errorf("got %dx%d", w, h)
	}
}

func TestBrokenPicture(t *testing.T) {
	if _, err := Make([]byte("not a picture at all")); err == nil {
		t.Error("no error for a broken picture")
	}
}

// withOrientation adds an EXIF block with orientation o to a JPEG.
func withOrientation(jpg []byte, o uint16) []byte {
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08")
	tiff = binary.BigEndian.AppendUint16(tiff, 1) // one tag
	tiff = binary.BigEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.BigEndian.AppendUint16(tiff, 3) // SHORT
	tiff = binary.BigEndian.AppendUint32(tiff, 1)
	tiff = binary.BigEndian.AppendUint16(tiff, o)
	tiff = append(tiff, 0, 0, 0, 0, 0, 0)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xFF, 0xE1}
	app1 = binary.BigEndian.AppendUint16(app1, uint16(len(seg)+2))
	app1 = append(app1, seg...)
	return append(append([]byte{0xFF, 0xD8}, app1...), jpg[2:]...)
}

func TestSidewaysPhotoTurnedUpright(t *testing.T) {
	var j bytes.Buffer
	jpeg.Encode(&j, picture(640, 480), &jpeg.Options{Quality: 95})
	// Orientation 6: the camera was turned; the picture must be rotated 90
	// degrees clockwise to show upright, so left (red) ends up on top.
	out, err := Make(withOrientation(j.Bytes(), 6))
	if err != nil {
		t.Fatal(err)
	}
	w, h, m := size(t, out)
	if w != 240 || h != 320 {
		t.Fatalf("got %dx%d, want 240x320", w, h)
	}
	top, bottom := m.At(w/2, 20), m.At(w/2, h-20)
	if r, _, b, _ := top.RGBA(); r < b {
		t.Error("top isn't red")
	}
	if r, _, b, _ := bottom.RGBA(); r > b {
		t.Error("bottom isn't blue")
	}
	if o := orientation(j.Bytes()); o != 1 {
		t.Errorf("plain JPEG orientation %d", o)
	}
}
