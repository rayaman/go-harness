package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadFileTool reads a text file from disk, scoped to a base directory so
// the model can't read anything outside it. This is the pattern to follow
// for any tool that touches the filesystem: validate/clamp the path before
// touching disk.
type ReadFileTool struct {
	BaseDir string
}

func (t ReadFileTool) Name() string { return "read_file" }

func (t ReadFileTool) Description() string {
	return fmt.Sprintf("Read a text file's contents. Paths are relative to %s.", t.BaseDir)
}

func (t ReadFileTool) Parameters() map[string]any {
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

func (t ReadFileTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Clamp the resolved path to BaseDir so arguments like "../../etc/passwd"
	// can't escape the sandboxed directory.
	full := filepath.Join(t.BaseDir, params.Path)
	rel, err := filepath.Rel(t.BaseDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory")
	}

	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}
	return string(data), nil
}
