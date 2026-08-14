package harness

import (
	"encoding/json"
)

// Tool is anything the model can call mid-conversation. Implement this
// interface to add a new capability - the agent loop and the client don't
// need any changes, you just Register() it in main.go.
type Tool interface {
	// Name must match ^[a-zA-Z0-9_-]+$ - most model servers are strict about this.
	Name() string

	// Description is shown to the model so it knows when (and when not)
	// to use the tool. Be specific - vague descriptions cause the model
	// to call tools at the wrong time, or never call them at all.
	Description() string

	// Parameters is a JSON Schema object describing the tool's arguments.
	Parameters() map[string]any

	// Execute runs the tool with the raw JSON arguments the model sent.
	// The returned string is fed straight back to the model as the
	// result, so keep it concise and readable.
	Execute(args json.RawMessage) (string, error)
}

// Registry keeps track of the tools available to a Harness.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool, keyed by its Name(). Registering a second tool
// with the same name overwrites the first.
func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

// Get looks up a tool by name; used internally when dispatching calls.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Schemas converts every registered tool into the JSON-schema format the
// model server expects in the request body.
func (r *Registry) Schemas() []ToolSchema {
	schemas := make([]ToolSchema, 0, len(r.tools))
	for _, t := range r.tools {
		schemas = append(schemas, ToolSchema{
			Type: "function",
			Function: FunctionSpec{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return schemas
}
