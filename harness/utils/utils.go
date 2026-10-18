package utils

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"

	"ai-harness/harness/config"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

var conf = config.GetConfig()
var ignoreFile *ignore.GitIgnore

// Max accepted image file size (20 MB), roughly what vision models tolerate.
const maxImageBytes = 20 * 1024 * 1024

// Supported image extensions (lowercase) mapped to their MIME types.
var imageMimeTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// IsSupportedImage reports whether path has one of the supported image
// extensions. Use it to fail fast with a clear message before handing the
// path to LoadImage.
func IsSupportedImage(path string) bool {
	return imageMimeTypes[strings.ToLower(filepath.Ext(path))] != ""
}

// LoadImage reads an image file from disk and returns its raw base64
// content (no data-URI prefix) and its detected MIME type. The caller
// composes the provider-appropriate format: Ollama native wants the
// raw base64 in Message.Images; OpenAI-compat composes a
// data:<mime>;base64,<b64> URI.
//
// This is the single choke point where image files become model-request
// payload: every image path a user attaches (REPL @file, workflow
// step media, Lua ask_media) flows through here via
// Harness.buildUserMessage.
func LoadImage(path string) (string, string, error) {
	return LoadImageScaled(path, 0)
}

// LoadImageScaled is LoadImage with optional downscaling: when maxDim
// > 0, images whose longest side exceeds maxDim pixels are scaled down
// proportionally (see resizeImageMaxDimension) before base64 encoding.
// maxDim <= 0 keeps LoadImage's exact byte-for-byte behavior.
// Harness.buildUserMessage passes Harness.Config.MaxImageDimension,
// which workflows set via config.image_max_dimension.
func LoadImageScaled(path string, maxDim int) (string, string, error) {
	mime := imageMimeTypes[strings.ToLower(filepath.Ext(path))]
	if mime == "" {
		return "", "", fmt.Errorf("LoadImage: unsupported image extension (png/jpg/jpeg/gif/webp): %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if len(data) > maxImageBytes {
		return "", "", fmt.Errorf("LoadImage: image too large (%d bytes, max %d bytes): %s", len(data), maxImageBytes, path)
	}
	data = resizeImageMaxDimension(data, maxDim)
	return base64.StdEncoding.EncodeToString(data), mime, nil
}

// resizeImageMaxDimension downscales an already-encoded image so its
// longest side equals maxDim pixels, preserving aspect ratio; images
// already within the cap are returned as the exact same bytes (no
// decode/re-encode round-trip, so zero quality loss). Nothing is ever
// upscaled.
//
// It never fails the caller: if maxDim <= 0, the bytes can't be decoded
// (webp, gif, or plain garbage - no decoder registered / decode error),
// the format has no stdlib encoder to re-encode it as itself, or the
// re-encode itself errors, the original bytes are returned unchanged.
// Scaling is best-effort; a request must never die over it.
//
// Resampling is a simple stdlib box filter (golang.org/x/image is not a
// dependency of this module); see boxScale for the quality tradeoff.
func resizeImageMaxDimension(data []byte, maxDim int) []byte {
	if maxDim <= 0 || len(data) == 0 {
		return data
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data // unparseable format: send the bytes out unchanged
	}
	// Only re-encode formats the stdlib can also write back, so PNG stays
	// PNG (alpha intact) and JPEG stays JPEG. Anything else passes through.
	if format != "jpeg" && format != "png" {
		return data
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	longest := w
	if h > longest {
		longest = h
	}
	if longest <= maxDim {
		return data // within cap: byte-identical passthrough
	}
	scale := float64(maxDim) / float64(longest)
	dstW := int(math.Round(float64(w) * scale))
	dstH := int(math.Round(float64(h) * scale))
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	scaled := boxScale(toNRGBA(img), dstW, dstH)
	var buf bytes.Buffer
	switch format {
	case "jpeg":
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: 90}); err != nil {
			return data
		}
	case "png":
		if err := png.Encode(&buf, scaled); err != nil {
			return data
		}
	}
	return buf.Bytes()
}

// toNRGBA normalizes any decoded image into a zero-origin *image.NRGBA
// (jpeg hands back *image.YCbCr, png a *image.NRGBA or paletted image).
func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok && n.Rect.Min.X == 0 && n.Rect.Min.Y == 0 {
		return n
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// boxScale resamples src to dstW x dstH by averaging every source pixel
// that overlaps each destination pixel (a box filter), on straight
// (non-premultiplied) channels. This is downscaling only in practice
// (callers shrink), where the box filter is a solid cheap resampler.
// Tradeoff vs golang.org/x/image's CatmullRom: slightly softer output
// and a rougher alpha average, but it keeps this repo dependency-free.
func boxScale(src *image.NRGBA, dstW, dstH int) *image.NRGBA {
	srcW, srcH := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	xRatio := float64(srcW) / float64(dstW)
	yRatio := float64(srcH) / float64(dstH)
	for dy := 0; dy < dstH; dy++ {
		y0 := int(math.Floor(float64(dy) * yRatio))
		y1 := int(math.Ceil(float64(dy+1) * yRatio))
		if y1 > srcH {
			y1 = srcH
		}
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			x0 := int(math.Floor(float64(dx) * xRatio))
			x1 := int(math.Ceil(float64(dx+1) * xRatio))
			if x1 > srcW {
				x1 = srcW
			}
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					p := src.PixOffset(x, y)
					r += uint64(src.Pix[p])
					g += uint64(src.Pix[p+1])
					b += uint64(src.Pix[p+2])
					a += uint64(src.Pix[p+3])
					n++
				}
			}
			q := dst.PixOffset(dx, dy)
			dst.Pix[q] = uint8(r / n)
			dst.Pix[q+1] = uint8(g / n)
			dst.Pix[q+2] = uint8(b / n)
			dst.Pix[q+3] = uint8(a / n)
		}
	}
	return dst
}

func MatchPatterns(file string) (bool, error) {
	err := compileIgnore()
	if err != nil {
		return false, err
	}
	slashPath := filepath.ToSlash(file)
	return ignoreFile.MatchesPath(slashPath), nil
}

func compileIgnore() error {
	var err error
	if conf.HotReload || ignoreFile == nil {
		ignoreFile, err = ignore.CompileIgnoreFile(conf.IgnoreFile)
		if err != nil {
			return err
		}
	}
	return nil
}
