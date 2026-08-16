# Lua Filesystem Module

This document describes the filesystem module available to Lua tools in the ai-harness project. The module provides safe, sandboxed file operations with path validation to prevent directory traversal attacks.

## Module Import

To use the filesystem module in your Lua tool, import it with:

```lua
local fs = require("filesystem")
```

## Available Functions

### `fs.read_file(path)`

Reads the entire contents of a file.

**Parameters:**
- `path` (string): Path to the file, relative to the configured base directory

**Returns:**
- `string`: File contents on success
- `nil, string`: Error message on failure

**Example:**
```lua
local content, err = fs.read_file("data/config.json")
if err then
    print("Error:", err)
    return nil, err
end
print("File contents:", content)
```

---

### `fs.write_file(path, content)`

Writes content to a file, creating parent directories if needed.

**Parameters:**
- `path` (string): Path to the file, relative to the configured base directory
- `content` (string): The content to write to the file

**Returns:**
- `string`: Success message on success
- `nil, string`: Error message on failure

**Example:**
```lua
local result, err = fs.write_file("output/log.txt", "New log entry\n")
if err then
    return nil, err
end
print(result)  -- "Successfully wrote 14 bytes to output/log.txt"
```

---

### `fs.read_file_partial(path, start_line, line_count)`

Reads a specific range of lines from a file.

**Parameters:**
- `path` (string): Path to the file, relative to the configured base directory
- `start_line` (number, optional): 1-based line number to start reading from (default: 1)
- `line_count` (number, optional): Maximum number of lines to read (default: 100)

**Returns:**
- `string`: Contents of the specified lines on success
- `nil, string`: Error message on failure

**Example:**
```lua
-- Read lines 10-20
local lines, err = fs.read_file_partial("data/records.txt", 10, 11)
if err then
    return nil, err
end
print("Lines 10-20:", lines)
```

---

### `fs.get_file_size(path)`

Gets file size and metadata without reading the file contents.

**Parameters:**
- `path` (string): Path to the file, relative to the configured base directory

**Returns:**
- `table`: Table with fields:
  - `path` (string): The requested path
  - `size_bytes` (number): File size in bytes
  - `human_readable` (string): Human-readable size (e.g., "1.2 MB")
  - `is_directory` (boolean): true if the path is a directory
- `nil, string`: Error message on failure

**Example:**
```lua
local info, err = fs.get_file_size("data/report.pdf")
if err then
    return nil, err
end
print("File size:", info.human_readable)
print("Is directory:", info.is_directory)
```

---

### `fs.edit_file(path, old_str, new_str)`

Replaces the first occurrence of a string in a file.

**Parameters:**
- `path` (string): Path to the file, relative to the configured base directory
- `old_str` (string): The exact string to replace
- `new_str` (string): The replacement string

**Returns:**
- `string`: Success message on success
- `nil, string`: Error message on failure

**Note:** If `old_str` appears more than once in the file, the operation fails to prevent accidental edits.

**Example:**
```lua
local result, err = fs.edit_file("config.yaml", "port: 8080", "port: 9000")
if err then
    return nil, err
end
print(result)  -- "Successfully edited config.yaml (replaced 1 occurrence)"
```

---

## Security Features

### Path Validation

All filesystem operations validate paths to prevent directory traversal attacks. For example, paths like `../../etc/passwd` are rejected.

### Base Directory Scoping

All operations are scoped to a configured base directory (default: "."). Files outside this directory cannot be accessed.

### Safe File Operations

- Files are written with standard permissions (0644)
- Parent directories are automatically created when writing files
- File operations are limited to text-based content

---

## Example: Complete Lua Tool with Filesystem Operations

```lua
-- Import required modules
local fs = require("filesystem")

function name()
    return "analyze_log"
end

function description()
    return "Reads the last 10 lines of a log file and returns them."
end

function parameters()
    return {
        type = "object",
        properties = {
            path = {
                type = "string",
                description = "Path to the log file, relative to the base directory."
            },
            line_count = {
                type = "integer",
                description = "Number of lines to read (default: 10).",
                default = 10
            }
        },
        required = {"path"}
    }
end

function execute(args)
    if not args.path or args.path == "" then
        return nil, "path argument is required"
    end

    -- Get file size first to check if it's reasonable
    local info, err = fs.get_file_size(args.path)
    if err then
        return nil, "failed to get file info: " .. err
    end

    -- Check file size (limit to 10MB)
    if info.size_bytes > 10 * 1024 * 1024 then
        return nil, "file too large (max 10MB)"
    end

    -- Read the last N lines
    local content, err = fs.read_file_partial(args.path, 1, args.line_count or 10)
    if err then
        return nil, "failed to read file: " .. err
    end

    -- Return the content
    return content, nil
end
```

---

## Configuration

The base directory for filesystem operations can be configured when creating the Lua tool:

```go
// In main.go
tool, err := luatool.NewLuaToolFromScriptWithConfig(
    scriptData, 
    luatool.FilesystemConfig{BaseDir: "data"}
)
```

If no configuration is provided, the default base directory is "." (the current directory).

---

## Best Practices

1. **Validate Input**: Always check that required arguments are present and valid before calling filesystem functions
2. **Check File Sizes**: For operations that might process large files, check the file size first
3. **Use Partial Reads**: When reading large files, use `read_file_partial` to avoid loading everything into memory
4. **Error Handling**: Always check for errors after filesystem operations and return meaningful error messages
5. **Security**: Never pass unvalidated user input directly to filesystem functions
