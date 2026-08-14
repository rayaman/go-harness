package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadPartialFileTool reads a specific line range from a text file on disk,
// scoped to a base directory to prevent path traversal.
type ReadPartialFileTool struct {
	BaseDir string
}

func (t ReadPartialFileTool) Name() string { return "read_file_partial" }

func (t ReadPartialFileTool) Description() string {
	return fmt.Sprintf("Read a specific range of lines from a text file. Paths are relative to %s.", t.BaseDir)
}

func (t ReadPartialFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file, relative to the allowed base directory.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "The 1-based line number to start reading from (defaults to 1).",
			},
			"line_count": map[string]any{
				"type":        "integer",
				"description": "Maximum number of lines to read starting from start_line (defaults to 100).",
			},
		},
		"required": []string{"path"},
	}
}

func (t ReadPartialFileTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		LineCount int    `json:"line_count"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Set sensible defaults
	if params.StartLine < 1 {
		params.StartLine = 1
	}
	if params.LineCount <= 0 {
		params.LineCount = 100 // Default batch limit
	}

	// Clamp the resolved path to BaseDir to prevent directory traversal
	full := filepath.Join(t.BaseDir, params.Path)
	rel, err := filepath.Rel(t.BaseDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory")
	}

	file, err := os.Open(full)
	if err != nil {
		return "", fmt.Errorf("opening file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Optional: Increase buffer size if handling lines longer than 64KB
	const maxCapacity = 1024 * 1024 // 1 MB line limit
	buf := make([]byte, maxCapacity)
	scanner.Buffer(buf, maxCapacity)

	var lines []string
	currentLine := 0
	endLine := params.StartLine + params.LineCount - 1

	for scanner.Scan() {
		currentLine++

		// Skip until we reach the start line
		if currentLine < params.StartLine {
			continue
		}

		// Stop once we pass the target window
		if currentLine > endLine {
			break
		}

		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading file stream: %w", err)
	}

	if len(lines) == 0 {
		if currentLine < params.StartLine {
			return "", fmt.Errorf("start_line %d is beyond end of file (total lines: %d)", params.StartLine, currentLine)
		}
		return "", nil
	}

	return strings.Join(lines, "\n"), nil
}
