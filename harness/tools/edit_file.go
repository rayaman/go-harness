package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EditFileTool edits an existing file by replacing a unique matching substring.
type EditFileTool struct {
	BaseDir string
}

func (t EditFileTool) Name() string { return "edit_file" }

func (t EditFileTool) Description() string {
	return fmt.Sprintf("Edit an existing file by replacing exact target text (old_str) with new text (new_str). Paths are relative to %s.", t.BaseDir)
}

func (t EditFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file, relative to the allowed base directory.",
			},
			"old_str": map[string]any{
				"type":        "string",
				"description": "The exact string segment currently in the file to be replaced.",
			},
			"new_str": map[string]any{
				"type":        "string",
				"description": "The new string to insert in place of old_str.",
			},
		},
		"required": []string{"path", "old_str", "new_str"},
	}
}

func (t EditFileTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Path   string `json:"path"`
		OldStr string `json:"old_str"`
		NewStr string `json:"new_str"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if params.OldStr == "" {
		return "", fmt.Errorf("old_str cannot be empty")
	}

	// Clamp the resolved path to BaseDir
	full := filepath.Join(t.BaseDir, params.Path)
	rel, err := filepath.Rel(t.BaseDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory")
	}

	// Read original contents
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("reading file for edit: %w", err)
	}

	content := string(data)

	// Ensure old_str exists in the target file
	count := strings.Count(content, params.OldStr)
	if count == 0 {
		return "", fmt.Errorf("target string (old_str) not found in %s", params.Path)
	}

	// Prevent accidental collateral damage if old_str isn't unique enough
	if count > 1 {
		return "", fmt.Errorf("target string (old_str) found %d times in %s; please provide more surrounding context to make it unique", count, params.Path)
	}

	// Perform replacement
	updatedContent := strings.Replace(content, params.OldStr, params.NewStr, 1)

	// Write back to disk
	if err := os.WriteFile(full, []byte(updatedContent), 0644); err != nil {
		return "", fmt.Errorf("saving edited file: %w", err)
	}

	return fmt.Sprintf("Successfully edited %s (replaced 1 occurrence)", params.Path), nil
}
