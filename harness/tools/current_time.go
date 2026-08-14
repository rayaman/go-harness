package tools

import (
	"encoding/json"
	"time"
)

// =============================================================================
// Example tools below. Copy this pattern to add your own - e.g. a web
// search tool, a database query tool, or a call into another local service.
// =============================================================================

// CurrentTimeTool returns the current local time and takes no arguments.
// It's a good "hello world" tool for confirming the loop works end-to-end.
type CurrentTimeTool struct{}

func (t CurrentTimeTool) Name() string        { return "current_time" }
func (t CurrentTimeTool) Description() string { return "Get the current local date and time." }

func (t CurrentTimeTool) Parameters() map[string]any {
	// No arguments needed, but the schema object itself is still required.
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t CurrentTimeTool) Execute(_ json.RawMessage) (string, error) {
	return time.Now().Format(time.RFC1123), nil
}
