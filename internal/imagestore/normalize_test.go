package imagestore

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

var opts = Options{MaxBytes: 300 << 10, MaxDim: 1024, MaxPixels: 25_000_000}

func noisy(w, h int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(1))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(r.Intn(256)), byte(r.Intn(256)), byte(r.Intn(256)), alpha
	}
	return img
}

func encPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func dims(t *testing.T, b []byte) (int, int, string) {
	t.Helper()
	c, f, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return c.Width, c.Height, f
}

func TestNormalizeShrinksAndEncodesJPEG(t *testing.T) {
	res, err := Normalize(encPNG(t, noisy(2000, 1000, 255)), opts)
	if err != nil {
		t.Fatal(err)
	}
	w, h, f := dims(t, res.Data)
	if f != "jpeg" || res.ContentType != "image/jpeg" || max(w, h) > 1024 || w != 1024 || h != 512 {
		t.Fatalf("got %dx%d %s %s", w, h, f, res.ContentType)
	}
	if len(res.Data) > opts.MaxBytes {
		t.Fatalf("size %d", len(res.Data))
	}
}

func TestNormalizeKeepsAlphaAsPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	img.SetNRGBA(1, 1, color.NRGBA{255, 0, 0, 128})
	res, err := Normalize(encPNG(t, img), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, f := dims(t, res.Data); f != "png" || res.ContentType != "image/png" {
		t.Fatalf("got %s", f)
	}
}

func TestNormalizeGIFFirstFrame(t *testing.T) {
	p := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	f1, f2 := image.NewPaletted(image.Rect(0, 0, 10, 10), p), image.NewPaletted(image.Rect(0, 0, 10, 10), p)
	for i := range f2.Pix {
		f2.Pix[i] = 1
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{f1, f2}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	res, err := Normalize(b.Bytes(), opts)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, _, bl, _ := img.At(5, 5).RGBA()
	if r < 0xc000 || bl > 0x4000 { // red, not the blue of the second frame (jpeg is lossy)
		t.Fatalf("not the first frame: %d %d", r, bl)
	}
}

// exifJPEG makes a jpeg with an EXIF orientation and a GPS-looking payload in APP1.
func exifJPEG(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	var j bytes.Buffer
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	binary.Write(&tiff, binary.BigEndian, uint16(42))
	binary.Write(&tiff, binary.BigEndian, uint32(8))
	binary.Write(&tiff, binary.BigEndian, uint16(1))
	binary.Write(&tiff, binary.BigEndian, uint16(0x0112))
	binary.Write(&tiff, binary.BigEndian, uint16(3))
	binary.Write(&tiff, binary.BigEndian, uint32(1))
	binary.Write(&tiff, binary.BigEndian, orientation)
	tiff.Write([]byte{0, 0})
	binary.Write(&tiff, binary.BigEndian, uint32(0))
	tiff.WriteString("GPSLatitude-secret")
	seg := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	out := []byte{0xff, 0xd8, 0xff, 0xe1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(seg)+2))
	out = append(out, seg...)
	return append(out, j.Bytes()[2:]...)
}

func TestNormalizeOrientationAndMetadata(t *testing.T) {
	// 40x20 image, left half red, right half blue; orientation 6 = rotate 90 clockwise -> 20x40, red on top
	src := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			if x < 20 {
				src.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 255})
			} else {
				src.SetNRGBA(x, y, color.NRGBA{0, 0, 255, 255})
			}
		}
	}
	in := exifJPEG(t, src, 6)
	if !bytes.Contains(in, []byte("GPSLatitude-secret")) {
		t.Fatal("test input has no metadata")
	}
	res, err := Normalize(in, opts)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(res.Data, []byte("GPSLatitude")) || bytes.Contains(res.Data, []byte("Exif")) {
		t.Fatal("metadata survived")
	}
	img, _, _ := image.Decode(bytes.NewReader(res.Data))
	if img.Bounds().Dx() != 20 || img.Bounds().Dy() != 40 {
		t.Fatalf("size %v", img.Bounds())
	}
	r, _, b, _ := img.At(10, 5).RGBA()
	r2, _, b2, _ := img.At(10, 35).RGBA()
	if r < 0xc000 || b > 0x4000 || b2 < 0xc000 || r2 > 0x4000 {
		t.Fatal("not rotated: top should be red, bottom blue")
	}
}

func TestNormalizeRejects(t *testing.T) {
	if _, err := Normalize([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), opts); err != ErrFormat {
		t.Fatalf("svg: %v", err)
	}
	if _, err := Normalize([]byte("hello"), opts); err != ErrFormat {
		t.Fatalf("text: %v", err)
	}
	if _, err := Normalize([]byte("\x89PNG\r\n\x1a\ntruncated"), opts); err != ErrFormat {
		t.Fatalf("truncated: %v", err)
	}
	// 4000x4000 is within a bigger limit but over this one; it is refused from the header alone
	small := Options{MaxBytes: opts.MaxBytes, MaxDim: 1024, MaxPixels: 1_000_000}
	if _, err := Normalize(encPNG(t, image.NewNRGBA(image.Rect(0, 0, 4000, 4000))), small); err != ErrPixels {
		t.Fatalf("pixels: %v", err)
	}
}

func TestNormalizeTooLarge(t *testing.T) {
	// noise does not compress: even a tiny limit cannot be met by shrinking down to the minimum
	if _, err := Normalize(encPNG(t, noisy(600, 600, 255)), Options{MaxBytes: 200, MaxDim: 1024, MaxPixels: 25_000_000}); err != ErrTooLarge {
		t.Fatalf("got %v", err)
	}
}

func TestNormalizeShrinksStepsToFit(t *testing.T) {
	res, err := Normalize(encPNG(t, noisy(1024, 1024, 255)), Options{MaxBytes: 60 << 10, MaxDim: 1024, MaxPixels: 25_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Data) > 60<<10 {
		t.Fatalf("size %d", len(res.Data))
	}
}
