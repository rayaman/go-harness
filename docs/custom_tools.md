# Custom Lua Tools

This project supports writing custom tools in Lua. Lua tools are scripts that define functions to be called by the AI model during conversations. They're loaded dynamically at runtime and can be hot-reloaded while the application is running.

## Overview

Custom Lua tools are stored in the `agent/custom_tools/` directory (configurable via `agent/config.yaml`). Each `.lua` file in this directory is treated as a separate tool that can be invoked by the AI model.

## Tool Structure

A Lua tool script must define four functions:

1. `name()` - Returns the tool's unique identifier
2. `description()` - Returns a human-readable description of what the tool does
3. `parameters()` - Returns a JSON Schema object describing the tool's arguments
4. `execute(args)` - The main function that performs the tool's logic

### Example: `agent/custom_tools/example.lua`

```lua
function name()
    return "reverse_text"
end

function description()
    return "Reverses a string and converts it to uppercase or lowercase."
end

function parameters()
    return {
        type = "object",
        properties = {
            text = {
                type = "string",
                description = "The target string to reverse."
            },
            uppercase = {
                type = "boolean",
                description = "Set to true to make the output uppercase."
            }
        },
        required = {"text"}
    }
end

function execute(args)
    if not args.text or args.text == "" then
        return nil, "text argument is required"
    end

    -- Reverse the string
    local rev = string.reverse(args.text)

    if args.uppercase then
        rev = string.upper(rev)
    end

    return rev, nil
end
```

## Function Details

### `name()`

Returns the tool's name, which must match the regex `^[a-zA-Z0-9_-]+$`. This name is used by the AI model to call the tool.

```lua
function name()
    return "my_custom_tool"
end
```

### `description()`

Returns a detailed description of what the tool does. This helps the AI model understand when and how to use the tool.

```lua
function description()
    return "Performs a specific task when called by the AI model."
end
```

### `parameters()`

Returns a JSON Schema object that describes the tool's arguments. This schema is sent to the AI model so it knows what arguments to provide.

```lua
function parameters()
    return {
        type = "object",
        properties = {
            text = {
                type = "string",
                description = "The text to process."
            },
            uppercase = {
                type = "boolean",
                description = "Convert to uppercase if true."
            }
        },
        required = {"text"}  -- List required arguments here
    }
end
```

### `execute(args)`

This is the main function that performs the tool's logic. It receives the arguments as a Lua table and must return two values:

1. The result (as a string) or `nil` on error
2. An error message string or `nil` on success

```lua
function execute(args)
    -- Check required arguments
    if not args.text or args.text == "" then
        return nil, "text argument is required"
    end

    -- Perform the tool's logic
    local result = string.upper(args.text)

    -- Return result and nil error
    return result, nil
end
```

## Execution Environment

Lua tools run in a sandboxed environment with access to:

- Standard Lua libraries (`string`, `math`, `table`, etc.)
- Custom HTTP module (see `harness/lua-tool/http.go`)
- Filesystem module (see `harness/lua-tool/filesystem.go`)
- Type conversion helpers between Lua and Go values

### Safe Execution

Tools execute in a secure sandbox that restricts access to potentially dangerous functions like `io.*`, `os.*`, `require`, and `dofile`. This prevents malicious code from accessing the file system or executing arbitrary commands.

## Filesystem Module

The filesystem module provides safe, sandboxed file operations with path validation to prevent directory traversal attacks.

### Importing the Module

```lua
local fs = require("filesystem")
```

### Available Functions

#### `fs.read_file(path)`

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
    return nil, "failed to read file: " .. err
end
print("File contents:", content)
```

#### `fs.write_file(path, content)`

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

#### `fs.read_file_partial(path, start_line, line_count)`

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

#### `fs.get_file_size(path)`

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

#### `fs.edit_file(path, old_str, new_str)`

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

### Complete Example

```lua
-- Example tool that processes a file
local fs = require("filesystem")

function name()
    return "uppercase_file"
end

function description()
    return "Reads a file, converts all text to uppercase, and writes it back."
end

function parameters()
    return {
        type = "object",
        properties = {
            path = {
                type = "string",
                description = "Path to the file, relative to the base directory."
            }
        },
        required = {"path"}
    }
end

function execute(args)
    if not args.path or args.path == "" then
        return nil, "path argument is required"
    end

    -- Read the file
    local content, err = fs.read_file(args.path)
    if err then
        return nil, "failed to read file: " .. err
    end

    -- Process the content
    local processed = string.upper(content)

    -- Write back to the same file
    local result, err = fs.write_file(args.path, processed)
    if err then
        return nil, "failed to write file: " .. err
    end

    return result, nil
end
```

## Hot Reload

When `hot-reload: true` is set in `agent/config.yaml`, the system automatically reloads Lua tools when their files are modified. This allows you to iterate on your tools without restarting the application.

## Examples

See the following example tools in `agent/custom_tools/`:

- `example.lua` - Reverses text and optionally converts to uppercase
- `math.lua` - Safely evaluates mathematical expressions
- `process_file.lua` - Demonstrates filesystem operations (read, process, write)

## Best Practices

1. **Validate Input**: Always check that required arguments are present and valid before processing
2. **Clear Descriptions**: Write detailed descriptions that help the AI model understand when to use your tool
3. **Error Handling**: Return meaningful error messages when something goes wrong
4. **Simple Results**: Keep return values concise and readable for the AI model
5. **Security**: Avoid accessing sensitive system resources or credentials in your tools

## Troubleshooting

- **Tool not loading**: Ensure the file has a `.lua` extension and is in the correct directory
- **Missing functions**: All four required functions (`name`, `description`, `parameters`, `execute`) must be defined
- **Syntax errors**: Check the console for error messages when the tool is loaded
- **Hot reload not working**: Ensure `hot-reload: true` is set in `agent/config.yaml`

## Integration with the Main Application

Lua tools are registered in `main.go` alongside built-in Go tools:

```go
// --- Dynamic Custom Lua Tools Loader -------------------------------
defaultConfigDir := "agent/custom_tools"
if conf.CustomToolDir != "" {
    defaultConfigDir = conf.CustomToolDir
}
customToolsDir := defaultConfigDir
entries, err := os.ReadDir(customToolsDir)
if err != nil {
    fmt.Printf("Note: Skipping custom Lua tools (could not read directory %q: %v)\n", customToolsDir, err)
} else {
    for _, entry := range entries {
        if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
            continue
        }

        scriptPath := filepath.Join(customToolsDir, entry.Name())
        data, err := os.ReadFile(scriptPath)
        if err != nil {
            fmt.Fprintf(os.Stderr, "Warning: failed to read custom tool %s: %v\n", scriptPath, err)
            continue
        }

        tool, err := luatool.NewLuaToolFromScriptWithConfig(
            string(data), 
            luatool.FilesystemConfig{BaseDir: "."}
        )
        if err != nil {
            fmt.Fprintf(os.Stderr, "Warning: failed to parse Lua tool %s: %v\n", scriptPath, err)
            continue
        }

        registry.Register(tool)
        fmt.Printf("Loaded custom Lua tool: %s\n", entry.Name())
    }
}
```

This integration allows Lua tools to be used seamlessly alongside Go-implemented tools in the AI harness.

### Filesystem Configuration

By default, Lua tools are configured to use the current directory (`.`) as the base directory for file operations. You can customize this by passing a different `FilesystemConfig`:

```go
// Configure Lua tools to use a specific base directory
tool, err := luatool.NewLuaToolFromScriptWithConfig(
    scriptData, 
    luatool.FilesystemConfig{BaseDir: "data"}
)
```

This ensures all file operations are scoped to the specified directory, preventing access to sensitive files outside the intended scope.
