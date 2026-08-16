-- Example Lua tool demonstrating filesystem operations
-- This tool reads a file, processes it, and writes the result back

-- Import the filesystem module
local fs = require("filesystem")

function name()
    return "process_file"
end

function description()
    return "Reads a file, converts text to uppercase, and writes it back."
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

    -- Read the file contents
    local content, err = fs.read_file(args.path)
    if err then
        return nil, "failed to read file: " .. err
    end

    -- Process the content (convert to uppercase)
    local processed = string.upper(content)

    -- Write back to the same file
    local result, err = fs.write_file(args.path, processed)
    if err then
        return nil, "failed to write file: " .. err
    end

    return result, nil
end
