package luatool

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// RegisterFilesystemModule attaches the custom 'filesystem' module to the Gopher-Lua state.
func RegisterFilesystemModule(L *lua.LState, cfg FilesystemConfig) {
	if cfg.BaseDir == "" {
		cfg.BaseDir = DefaultFilesystemConfig.BaseDir
	}

	// Define module functions
	exports := map[string]lua.LGFunction{
		"read_file":         makeReadFileFunc(cfg),
		"write_file":        makeWriteFileFunc(cfg),
		"read_file_partial": makeReadPartialFileFunc(cfg),
		"get_file_size":     makeGetFileSizeFunc(cfg),
		"edit_file":         makeEditFileFunc(cfg),
	}

	// Register module so scripts can use `local fs = require("filesystem")`.
	// The preload loader must PUSH the module table: SetFuncs only fills it,
	// and the loader's stack top is otherwise the module-name string, which
	// require() would then hand back as the "module".
	L.PreloadModule("filesystem", func(L *lua.LState) int {
		tbl := L.NewTable()
		L.SetFuncs(tbl, exports)
		L.Push(tbl)
		return 1
	})
}

// validatePath ensures the path stays within the base directory.
func validatePath(baseDir, requestedPath string) (string, error) {
	// Join base and requested path
	fullPath := filepath.Join(baseDir, requestedPath)

	// Get relative path to detect traversal attempts
	rel, err := filepath.Rel(baseDir, fullPath)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}

	// Check for path traversal
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory")
	}

	return fullPath, nil
}

// makeReadFileFunc creates a function to read a file's contents
func makeReadFileFunc(cfg FilesystemConfig) lua.LGFunction {
	return func(L *lua.LState) int {
		path := L.CheckString(1)

		// Validate path
		fullPath, err := validatePath(cfg.BaseDir, path)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("path validation error: %v", err)))
			return 2
		}

		// Read file
		data, err := os.ReadFile(fullPath)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to read file: %v", err)))
			return 2
		}

		L.Push(lua.LString(string(data)))
		L.Push(lua.LNil)
		return 2
	}
}

// makeWriteFileFunc creates a function to write to a file
func makeWriteFileFunc(cfg FilesystemConfig) lua.LGFunction {
	return func(L *lua.LState) int {
		path := L.CheckString(1)
		content := L.CheckString(2)

		// Validate path
		fullPath, err := validatePath(cfg.BaseDir, path)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("path validation error: %v", err)))
			return 2
		}

		// Create parent directories if they don't exist
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to create directory: %v", err)))
			return 2
		}

		// Write file
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to write file: %v", err)))
			return 2
		}

		L.Push(lua.LString(fmt.Sprintf("Successfully wrote %d bytes to %s", len(content), path)))
		L.Push(lua.LNil)
		return 2
	}
}

// makeReadPartialFileFunc creates a function to read a specific range of lines from a file
func makeReadPartialFileFunc(cfg FilesystemConfig) lua.LGFunction {
	return func(L *lua.LState) int {
		path := L.CheckString(1)
		startLine := 1
		lineCount := 100

		// Parse optional parameters
		if L.GetTop() >= 2 {
			startLine = int(L.CheckNumber(2))
		}
		if L.GetTop() >= 3 {
			lineCount = int(L.CheckNumber(3))
		}

		// Validate path
		fullPath, err := validatePath(cfg.BaseDir, path)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("path validation error: %v", err)))
			return 2
		}

		// Open file
		file, err := os.Open(fullPath)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to open file: %v", err)))
			return 2
		}
		defer file.Close()

		// Set defaults
		if startLine < 1 {
			startLine = 1
		}
		if lineCount <= 0 {
			lineCount = 100
		}

		// Read file line by line
		scanner := bufio.NewScanner(file)
		const maxCapacity = 1024 * 1024 // 1 MB line limit
		buf := make([]byte, maxCapacity)
		scanner.Buffer(buf, maxCapacity)

		var lines []string
		currentLine := 0
		endLine := startLine + lineCount - 1

		for scanner.Scan() {
			currentLine++

			// Skip until we reach the start line
			if currentLine < startLine {
				continue
			}

			// Stop once we pass the target window
			if currentLine > endLine {
				break
			}

			lines = append(lines, scanner.Text())
		}

		if err := scanner.Err(); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("error reading file: %v", err)))
			return 2
		}

		// Return result
		if len(lines) == 0 {
			if currentLine < startLine {
				L.Push(lua.LNil)
				L.Push(lua.LString(fmt.Sprintf("start_line %d is beyond end of file (total lines: %d)", startLine, currentLine)))
				return 2
			}
			L.Push(lua.LString(""))
			L.Push(lua.LNil)
			return 2
		}

		L.Push(lua.LString(strings.Join(lines, "\n")))
		L.Push(lua.LNil)
		return 2
	}
}

// makeGetFileSizeFunc creates a function to get file size and metadata
func makeGetFileSizeFunc(cfg FilesystemConfig) lua.LGFunction {
	return func(L *lua.LState) int {
		path := L.CheckString(1)

		// Validate path
		fullPath, err := validatePath(cfg.BaseDir, path)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("path validation error: %v", err)))
			return 2
		}

		// Get file info
		info, err := os.Stat(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				L.Push(lua.LNil)
				L.Push(lua.LString(fmt.Sprintf("file does not exist: %s", path)))
				return 2
			}
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to get file info: %v", err)))
			return 2
		}

		// Create result table
		result := L.NewTable()
		result.RawSetString("path", lua.LString(path))
		result.RawSetString("size_bytes", lua.LNumber(info.Size()))
		result.RawSetString("human_readable", lua.LString(formatBytes(info.Size())))
		result.RawSetString("is_directory", lua.LBool(info.IsDir()))

		L.Push(result)
		L.Push(lua.LNil)
		return 2
	}
}

// makeEditFileFunc creates a function to edit a file by replacing text
func makeEditFileFunc(cfg FilesystemConfig) lua.LGFunction {
	return func(L *lua.LState) int {
		path := L.CheckString(1)
		oldStr := L.CheckString(2)
		newStr := L.CheckString(3)

		// Validate path
		fullPath, err := validatePath(cfg.BaseDir, path)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("path validation error: %v", err)))
			return 2
		}

		// Read original contents
		data, err := os.ReadFile(fullPath)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to read file: %v", err)))
			return 2
		}

		content := string(data)

		// Ensure old_str exists in the target file
		count := strings.Count(content, oldStr)
		if count == 0 {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("target string (old_str) not found in %s", path)))
			return 2
		}

		// Prevent accidental collateral damage if old_str isn't unique enough
		if count > 1 {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("target string (old_str) found %d times in %s; please provide more surrounding context to make it unique", count, path)))
			return 2
		}

		// Perform replacement
		updatedContent := strings.Replace(content, oldStr, newStr, 1)

		// Write back to disk
		if err := os.WriteFile(fullPath, []byte(updatedContent), 0644); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to write file: %v", err)))
			return 2
		}

		L.Push(lua.LString(fmt.Sprintf("Successfully edited %s (replaced 1 occurrence)", path)))
		L.Push(lua.LNil)
		return 2
	}
}

// Helper function to turn raw byte values into human-readable strings
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
