package utils

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"
)

// encodePNG/encodeJPEG build tiny in-memory images so the scaler tests
// need no fixture files and no network.

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// solidImage builds an opaque image of the given size (blue) so decoded
// results can be sanity-checked without pixel-perfect expectations.
func solidImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 120, B: 220, A: 255})
		}
	}
	return img
}

func decodeDims(t *testing.T, data []byte) (w, h int, format string) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy(), format
}

func TestResizeShrinksOversizedJPEG(t *testing.T) {
	orig := encodeJPEG(t, solidImage(300, 200))
	out := resizeImageMaxDimension(orig, 100)

	w, h, format := decodeDims(t, out)
	if format != "jpeg" {
		t.Errorf("re-encode format: got %q, want jpeg", format)
	}
	if w != 100 || h != 67 { // scale = 100/300; round(200*1/3) = 67
		t.Errorf("dims: got %dx%d, want 100x67", w, h)
	}
	// Aspect ratio preserved within rounding error.
	if got, want := float64(w)/float64(h), 1.5; math.Abs(got-want) > 0.02 {
		t.Errorf("aspect ratio drifted: got %.3f, want ~%.3f", got, want)
	}
	if bytes.Equal(out, orig) {
		t.Error("oversized image should not pass through unchanged")
	}
}

func TestResizeWideImageLongestSideHitsCap(t *testing.T) {
	out := resizeImageMaxDimension(encodePNG(t, solidImage(400, 100)), 200)
	w, h, format := decodeDims(t, out)
	if format != "png" {
		t.Errorf("re-encode format: got %q, want png", format)
	}
	if w != 200 || h != 50 {
		t.Errorf("dims: got %dx%d, want 200x50", w, h)
	}
}

func TestResizeAtOrBelowCapReturnsIdenticalBytes(t *testing.T) {
	atCap := encodePNG(t, solidImage(120, 80))
	if out := resizeImageMaxDimension(atCap, 120); !bytes.Equal(out, atCap) {
		t.Error("image exactly at the cap must be byte-identical (no re-encode)")
	}
	below := encodeJPEG(t, solidImage(119, 40))
	if out := resizeImageMaxDimension(below, 120); !bytes.Equal(out, below) {
		t.Error("image below the cap must be byte-identical (no re-encode)")
	}
	// maxDim <= 0 disables scaling entirely, even for oversized input.
	oversized := encodePNG(t, solidImage(300, 300))
	if out := resizeImageMaxDimension(oversized, 0); !bytes.Equal(out, oversized) {
		t.Error("maxDim 0 must disable scaling (byte-identical)")
	}
}

func TestResizeAlphaPNGStaysPNGWithAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			img.Set(x, y, color.NRGBA{R: 200, G: 30, B: 30, A: uint8(x % 256)})
		}
	}
	out := resizeImageMaxDimension(encodePNG(t, img), 100)

	decoded, format, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("alpha PNG must stay PNG, got format %q", format)
	}
	b := decoded.Bounds()
	if b.Dx() != 100 || b.Dy() != 100 {
		t.Errorf("dims: got %dx%d, want 100x100", b.Dx(), b.Dy())
	}
	// The source alpha varies with x, so a box average near x=10 must be
	// far below opaque: alpha was carried through instead of flattened.
	_, _, _, a := decoded.At(b.Min.X+10, b.Min.Y+50).RGBA()
	if a > 0x2000 {
		t.Errorf("left-side alpha should be well below opaque, got a=%#04x", a)
	}
}

func TestResizeGarbagePassesThrough(t *testing.T) {
	garbage := []byte("this is definitely not an image")
	if out := resizeImageMaxDimension(garbage, 64); !bytes.Equal(out, garbage) {
		t.Error("unparseable bytes must be returned unchanged, not an error")
	}
	if out := resizeImageMaxDimension(nil, 64); out != nil {
		t.Error("nil input must be returned unchanged")
	}
}
