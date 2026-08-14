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