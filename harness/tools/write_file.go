package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteFileTool creates or completely overwrites a file on disk within BaseDir.
type WriteFileTool struct {
	BaseDir string
}

func (t WriteFileTool) Name() string { return "write_file" }

// Serial reports true: writing files has side effects, so this tool never
// runs concurrently with other tool calls in a turn (harness.SerialTyper,
// roadmap 2.1).
func (t WriteFileTool) Serial() bool { return true }

func (t WriteFileTool) Description() string {
	return fmt.Sprintf("Create a new file or completely overwrite an existing file. Paths are relative to %s.", t.BaseDir)
}

func (t WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file, relative to the allowed base directory.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The exact content to write to the file.",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t WriteFileTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Clamp the resolved path to BaseDir
	full := filepath.Join(t.BaseDir, params.Path)
	rel, err := filepath.Rel(t.BaseDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory")
	}

	// Automatically create parent directories if they don't exist
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating directory structure: %w", err)
	}

	// Write file (0644 standard readable/writable file mode)
	if err := os.WriteFile(full, []byte(params.Content), 0644); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(params.Content), params.Path), nil
}
