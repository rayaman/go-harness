-- Programmatic workflow entry point. The `harness` global (and
-- require("harness")) is provided by the Go runner; `http`,
-- `filesystem`, and `json` come from the existing luatool modules.

local models, err = harness.list_models()
if err then
   harness.log("could not list models:", err)
   os.exit(1)
end

harness.log(string.format("server reports %d model(s):", #models))
local names = {}
for i, m in ipairs(models) do
   names[i] = m.id
   local size = m.size_human or "?"
   harness.log(string.format("  %2d. %-40s %s", i, m.id, size))
end

-- Ask the server for each model's real context window when available.
local rows = {}
for _, m in ipairs(models) do
   local n, cerr = harness.context_length(m.id)
   if not cerr then
      rows[#rows + 1] = string.format("%s: %d tokens", m.id, n)
   end
end
if #rows > 0 then
   harness.log("context windows:")
   for _, r in ipairs(rows) do
      harness.log("  " .. r)
   end
end

-- One LLM turn summarizing what we found. args are extra CLI args:
--   go run main.go --workflow workflows/models-report -- "focus on vision models"
local focus = harness.args[1] or "all of them"
local answer, aerr = harness.ask(
   "Here are the models served by my local model server:\n" ..
   table.concat(names, "\n") .. "\n\n" ..
   "Context window details: " .. table.concat(rows, "; ") .. "\n\n" ..
   "Give a short recommendation report about " .. focus .. ".",
   { reset = true, system = "You are a concise systems analyst." })
if aerr then
   harness.log("ask failed:", aerr)
   os.exit(1)
end
harness.log("\n--- report ---\n" .. answer)
