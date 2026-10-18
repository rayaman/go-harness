package utils

import (
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
func LoadImage(path string) (string, string, error) {
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
	return base64.StdEncoding.EncodeToString(data), mime, nil
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
