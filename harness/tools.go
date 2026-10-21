package harness

import (
	"encoding/json"
	"slices"
	"strings"
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

// SerialTyper is an optional interface a Tool can implement to opt out of
// the concurrent tool dispatch added by roadmap 2.1: a tool reporting
// Serial() == true is never executed alongside other tool calls from the
// same assistant turn - it runs one at a time, in call order. Tools whose
// Execute has side effects or shares state that is not goroutine-safe
// (write_file, edit_file, run_shell_command, the Lua tools - a
// gopher-lua VM is not goroutine-safe) report true. A tool that does not
// implement this interface, or reports false, is treated as
// concurrency-safe and may run in the bounded parallel pool.
type SerialTyper interface {
	Serial() bool
}

// MaxOutputTyper is an optional interface a Tool can implement to override
// Config.MaxToolOutputBytes (the cap on the tool's result string fed back
// to the model; see Harness.runTool) for that one tool. Tools that know
// their output is worth keeping whole - or that want to opt out of the
// global cap - return their own limit here; returning 0 means unlimited
// for that tool, regardless of the configured global cap.
type MaxOutputTyper interface {
	MaxOutputBytes() int
}

// Policy decides what Harness.runTool does with a tool call before
// Execute (roadmap 1.2).
type Policy int

const (
	// PolicyAllow runs the tool unconditionally. It is the zero value,
	// so a Registry with no policies configured behaves exactly as
	// before gates existed.
	PolicyAllow Policy = iota
	// PolicyAsk routes the call through Harness.Confirm before running
	// it; with no Confirm hook set the call is allowed (with a one-time
	// stderr warning).
	PolicyAsk
	// PolicyDeny refuses the call with a model-visible error string.
	PolicyDeny
)

// ToolPolicy is one name+policy pair from Registry.Policies.
type ToolPolicy struct {
	Name   string
	Policy Policy
}

// Registry keeps track of the tools available to a Harness.
type Registry struct {
	tools map[string]Tool

	// policies gates tool calls by name (roadmap 1.2). A missing entry
	// means PolicyAllow, so nil-safe lookups keep the no-policies
	// behavior. Register never touches policies, so hot-reloading a
	// tool (re-Registering the same name) keeps the policy set for it.
	policies map[string]Policy
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

// SetPolicy assigns a tool's permission policy. Policies can be set
// before or after the tool is registered; Register never clobbers them.
func (r *Registry) SetPolicy(name string, p Policy) {
	if r.policies == nil {
		r.policies = make(map[string]Policy)
	}
	r.policies[name] = p
}

// PolicyFor returns a tool's policy; unconfigured names are PolicyAllow.
func (r *Registry) PolicyFor(name string) Policy {
	return r.policies[name] // nil map reads as zero value = PolicyAllow
}

// ApplyPolicies replaces the whole policy mapping from configured
// allow/ask/deny name lists: on a name collision, deny beats ask beats
// allow. Calling it again with fresh lists (config hot-reload) fully
// replaces the previous mapping, so removed entries revert to allow.
func (r *Registry) ApplyPolicies(allow, ask, deny []string) {
	policies := make(map[string]Policy, len(allow)+len(ask)+len(deny))
	for _, n := range allow {
		policies[n] = PolicyAllow
	}
	for _, n := range ask {
		policies[n] = PolicyAsk
	}
	for _, n := range deny {
		policies[n] = PolicyDeny
	}
	r.policies = policies
}

// Policies returns every registered tool with its effective policy,
// sorted by name (for the future /tools slash command).
func (r *Registry) Policies() []ToolPolicy {
	out := make([]ToolPolicy, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, ToolPolicy{Name: name, Policy: r.PolicyFor(name)})
	}
	slices.SortFunc(out, func(a, b ToolPolicy) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
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
