package tools

import (
	"ai-harness/harness/utils"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ListFilesTool lists files in a directory, scoped to a base directory so
// the model can't access anything outside it. This is the pattern to follow
// for any tool that touches the filesystem: validate/clamp the path before
// touching disk.
type ListFilesTool struct {
	BaseDir string
}

func (t ListFilesTool) Name() string { return "list_files" }

func (t ListFilesTool) Description() string {
	return fmt.Sprintf("List files in a directory. Paths are relative to %s.", t.BaseDir)
}

func (t ListFilesTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the directory, relative to the allowed base directory. If empty or '/', lists the base directory.",
			},
			"recursive": map[string]any{
				"type":        "boolean",
				"description": "Whether to list files recursively. Defaults to false.",
				"default":     false,
			},
			"_omit_hidden": map[string]any{
				"type":        "boolean",
				"description": "Hidden flag to omit hidden files and directories (starting with '.').",
				"default":     false,
				"hidden":      true,
			},
		},
		"required": []string{"path"},
	}
}

func (t ListFilesTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Path       string `json:"path"`
		Recursive  bool   `json:"recursive"`
		OmitHidden bool   `json:"_omit_hidden"`
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

	// Verify the path is a directory
	info, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("reading directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}

	var files []string
	if params.Recursive {
		err = filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				relPath, _ := filepath.Rel(t.BaseDir, path)
				// Skip hidden files if omit_hidden is true
				if !params.OmitHidden || !isHidden(path) {
					files = append(files, relPath)
				}
			} else {
				// Skip hidden directories if omit_hidden is true
				if params.OmitHidden && isHidden(path) {
					return filepath.SkipDir
				}
			}
			return nil
		})
	} else {
		entries, err := os.ReadDir(full)
		if err != nil {
			return "", fmt.Errorf("reading directory entries: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				if !params.OmitHidden || !isHidden(filepath.Join(full, entry.Name())) {
					files = append(files, entry.Name())
				}
			} else if !params.OmitHidden || !isHidden(filepath.Join(full, entry.Name())) {
				// Include directories in listing if not omitting hidden
				files = append(files, entry.Name()+"/")
			}
		}
	}

	result, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling result: %w", err)
	}
	return string(result), nil
}

// isHidden checks if a file or directory name is considered hidden (starts with '.')
func isHidden(name string) bool {
	b, _ := utils.MatchPatterns(name)
	return b
}
