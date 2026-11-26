// Package filestore keeps the files of the cards (images, audio) as <dir>/<user id>/<id>.<ext>, and checks and
// normalizes what a user uploads.
package filestore

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif" // decoder only
	"image/jpeg"
	"image/png"
	"math"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decoder only
)

var (
	// ErrImageFormat: not a png, jpeg, gif or webp image (svg is refused on purpose: it can carry scripts).
	ErrImageFormat = errors.New("filestore: not a supported image")
	// ErrPixels: the image has more pixels than the limit; it is refused before it is decoded.
	ErrPixels = errors.New("filestore: image has too many pixels")
	// ErrImageTooLarge: the image is still larger than the limit after it was shrunk as far as allowed.
	ErrImageTooLarge = errors.New("filestore: image is too large")
)

// ImageOptions of NormalizeImage.
type ImageOptions struct {
	// MaxBytes is the largest result.
	MaxBytes int
	// MaxDim is the largest side of the result.
	MaxDim int
	// MaxPixels is the largest width x height accepted.
	MaxPixels int
}

// ImageResult is a normalized image: the bytes of a jpeg (opaque images) or a png (images with transparency)
// without any metadata.
type ImageResult struct {
	Data        []byte
	ContentType string
}

const (
	jpegQuality = 82
	minDim      = 128
)

// sniffImage returns the format by the magic bytes, "" when it is none of the supported ones.
func sniffImage(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "gif"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	}
	return ""
}

// NormalizeImage checks the bytes, shrinks the image to MaxDim, turns it upright by its EXIF orientation and
// encodes it again, which drops the metadata (EXIF, GPS). An animated gif keeps its first frame. The result
// is at most MaxBytes: when it is larger the quality and then the size go down by steps; if it never fits
// the error is ErrImageTooLarge.
func NormalizeImage(data []byte, o ImageOptions) (*ImageResult, error) {
	format := sniffImage(data)
	if format == "" {
		return nil, ErrImageFormat
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 {
		return nil, ErrImageFormat
	}
	if o.MaxPixels > 0 && cfg.Width*cfg.Height > o.MaxPixels {
		return nil, ErrPixels
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrImageFormat
	}
	orientation := 1
	if format == "jpeg" {
		orientation = jpegOrientation(data)
	}
	alpha := !isOpaque(src)

	dim := o.MaxDim
	if dim < minDim {
		dim = minDim
	}
	img := fit(src, dim)
	img = orient(img, orientation)

	qualities := []int{jpegQuality, 70, 58, 45}
	for {
		for _, q := range qualities {
			out, ct, err := encode(img, alpha, q)
			if err != nil {
				return nil, err
			}
			if o.MaxBytes <= 0 || len(out) <= o.MaxBytes {
				return &ImageResult{Data: out, ContentType: ct}, nil
			}
			if alpha { // png has no quality: only the size helps
				break
			}
		}
		b := img.Bounds()
		long := max(b.Dx(), b.Dy())
		if long <= minDim {
			return nil, ErrImageTooLarge
		}
		img = fit(img, max(minDim, int(math.Round(float64(long)*0.75))))
	}
}

func encode(img image.Image, alpha bool, quality int) ([]byte, string, error) {
	var buf bytes.Buffer
	if alpha {
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/png", nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/jpeg", nil
}

// isOpaque reports whether no pixel is transparent.
func isOpaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

// fit scales the image down so that its longest side is at most dim; it is always copied to an NRGBA image
// (the first frame of a gif is paletted).
func fit(src image.Image, dim int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > dim || h > dim {
		if w >= h {
			h = max(1, int(math.Round(float64(h)*float64(dim)/float64(w))))
			w = dim
		} else {
			w = max(1, int(math.Round(float64(w)*float64(dim)/float64(h))))
			h = dim
		}
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	}
	return dst
}

// orient applies the EXIF orientation (1 to 8) so that the pixels are upright.
func orient(src *image.NRGBA, o int) *image.NRGBA {
	if o < 2 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
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
			dst.SetNRGBA(nx, ny, src.NRGBAAt(x, y))
		}
	}
	return dst
}

// jpegOrientation reads the EXIF orientation of a jpeg (1 when there is none or it cannot be read).
func jpegOrientation(b []byte) int {
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xff {
			return 1
		}
		marker := b[i+1]
		if marker == 0xda || marker == 0xd9 { // start of scan: no more headers
			return 1
		}
		if marker == 0xff { // fill byte
			i++
			continue
		}
		size := int(b[i+2])<<8 | int(b[i+3])
		if size < 2 || i+2+size > len(b) {
			return 1
		}
		if marker == 0xe1 && size >= 8 && bytes.HasPrefix(b[i+4:], []byte("Exif\x00\x00")) {
			return exifOrientation(b[i+10 : i+2+size])
		}
		i += 2 + size
	}
	return 1
}

// exifOrientation reads tag 0x0112 of the first IFD of a TIFF block.
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var u16 func([]byte) int
	var u32 func([]byte) int
	switch string(t[:2]) {
	case "II":
		u16 = func(p []byte) int { return int(p[0]) | int(p[1])<<8 }
		u32 = func(p []byte) int { return int(p[0]) | int(p[1])<<8 | int(p[2])<<16 | int(p[3])<<24 }
	case "MM":
		u16 = func(p []byte) int { return int(p[0])<<8 | int(p[1]) }
		u32 = func(p []byte) int { return int(p[0])<<24 | int(p[1])<<16 | int(p[2])<<8 | int(p[3]) }
	default:
		return 1
	}
	off := u32(t[4:8])
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := u16(t[off:])
	for k := range n {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if u16(t[e:]) == 0x0112 {
			if v := u16(t[e+8:]); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}
