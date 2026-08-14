function name()
    return "execute_math"
end

function description()
    return "Evaluates mathematical expressions safely. Supports arithmetic (+, -, *, /, ^, %), common functions (sqrt, sin, cos, tan, log, abs, ceil, floor), and constants (pi, e)."
end

function parameters()
    return {
        type = "object",
        properties = {
            expression = {
                type = "string",
                description = "The mathematical expression to evaluate (e.g., 'sqrt(16) + 2^3', 'sin(pi / 4)', or 'log(100) * abs(-5)')."
            }
        },
        required = {"expression"}
    }
end

-- Strictly allowed functions and constants inside the evaluation sandbox
local safe_env = {
    -- Math Constants
    pi = math.pi,
    e  = math.exp(1),

    -- Math Functions
    abs   = math.abs,
    acos  = math.acos,
    asin  = math.asin,
    atan  = math.atan,
    ceil  = math.ceil,
    cos   = math.cos,
    deg   = math.deg,
    exp   = math.exp,
    floor = math.floor,
    fmod  = math.fmod,
    log   = math.log,
    max   = math.max,
    min   = math.min,
    pow   = math.pow,
    rad   = math.rad,
    sin   = math.sin,
    sqrt  = math.sqrt,
    tan   = math.tan
}

function execute(args)
    if not args.expression or args.expression == "" then
        return nil, "expression argument is required"
    end

    local expr = args.expression

    -- Basic check to reject common global access attempts
    if expr:find("io%.") or expr:find("os%.") or expr:find("require") or expr:find("dofile") then
        return nil, "invalid character or keyword in expression"
    end

    -- Compile the expression into a executable Lua function
    local chunk, compileErr = loadstring("return " .. expr)
    if not chunk then
        return nil, "syntax error in expression: " .. tostring(compileErr)
    end

    -- Sandbox: Restrict chunk environment so it can ONLY access safe_env
    setfenv(chunk, safe_env)

    -- Safely execute the compiled math chunk
    local success, result = pcall(chunk)
    if not success then
        return nil, "runtime evaluation error: " .. tostring(result)
    end

    if result == nil then
        return nil, "expression did not evaluate to a value"
    end

    -- Handle special IEEE floating-point cases (like NaN)
    if type(result) == "number" and result ~= result then
        return "NaN", nil
    end

    return tostring(result), nil
end