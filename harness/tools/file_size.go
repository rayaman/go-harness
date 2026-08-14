package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileInfoResult holds the JSON response payload for file information.
type FileInfoResult struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"size_bytes"`
	HumanReadable string `json:"human_readable"`
	IsDirectory   bool   `json:"is_directory"`
}

// GetFileSizeTool retrieves the size and basic metadata of a file.
type GetFileSizeTool struct {
	BaseDir string
}

func (t GetFileSizeTool) Name() string { return "get_file_size" }

func (t GetFileSizeTool) Description() string {
	return fmt.Sprintf("Get the file size (in bytes and human-readable format) and metadata without reading its contents. Paths are relative to %s.", t.BaseDir)
}

func (t GetFileSizeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file, relative to the allowed base directory.",
			},
		},
		"required": []string{"path"},
	}
}

func (t GetFileSizeTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Path string `json:"path"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Clamp the resolved path to BaseDir to prevent directory traversal
	full := filepath.Join(t.BaseDir, params.Path)
	rel, err := filepath.Rel(t.BaseDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory")
	}

	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file does not exist: %s", params.Path)
		}
		return "", fmt.Errorf("getting file info: %w", err)
	}

	result := FileInfoResult{
		Path:          params.Path,
		SizeBytes:     info.Size(),
		HumanReadable: formatBytes(info.Size()),
		IsDirectory:   info.IsDir(),
	}

	responseJSON, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling response: %w", err)
	}

	return string(responseJSON), nil
}

// Helper function to turn raw byte values into human-readable strings (e.g. 1.2 MB)
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
