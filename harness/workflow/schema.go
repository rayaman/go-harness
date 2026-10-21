package workflow

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

// schema.go implements the small stdlib-only JSON-Schema validator behind
// a step's optional `schema:` / `schema_file:` keys (roadmap 2.5). The
// repo has stayed dependency-free, so instead of a full JSON-Schema
// library this file supports exactly this documented subset:
//
//   - type: "object" | "array" | "string" | "number" | "integer" |
//     "boolean", either as a string or as an array of those names.
//     "integer" rejects non-integral numbers. Unrecognized type names
//     are ignored (forward-compatible).
//   - properties: recurses into keys that are present
//   - required: array of property names that must be present
//   - items: a single schema applied to every element of an array
//   - enum: array; membership compared with reflect.DeepEqual
//   - additionalProperties: only honored when boolean false, in which
//     case keys not listed under properties are rejected (and listed)
//
// Every other schema keyword (description, minLength, $id, ...) is
// ignored silently, so schemas written for stricter validators still
// load and validate what this subset can check.

// validateAgainstSchema checks value (the shape json.Unmarshal produces
// into any) against schema and returns human-readable, path-prefixed
// problems such as `title: expected string, got number` or
// `missing required property "author"`. An empty/nil result means valid.
func validateAgainstSchema(value any, schema map[string]any) []string {
	return validateValue(value, schema, "")
}

func validateValue(value any, schema map[string]any, path string) []string {
	if spec, ok := schema["type"]; ok && !typeOK(value, spec) {
		// Report only the type mismatch: structural keywords below
		// would cascade into noise once the shape is already wrong.
		return []string{schemaMsg(path, fmt.Sprintf("expected %s, got %s",
			typeDesc(spec), jsonKind(value)))}
	}

	var errs []string

	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, alt := range enum {
			if reflect.DeepEqual(value, alt) {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, schemaMsg(path, fmt.Sprintf("value %s not in enum %s",
				jsonRepr(value), jsonRepr(enum))))
		}
	}

	switch v := value.(type) {
	case map[string]any:
		if req, ok := schema["required"].([]any); ok {
			for _, item := range req {
				name, ok := item.(string)
				if !ok {
					continue
				}
				if _, present := v[name]; !present {
					errs = append(errs, schemaMsg(path, fmt.Sprintf("missing required property %q", name)))
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for _, name := range sortedKeys(props) {
			sub, ok := props[name].(map[string]any)
			if !ok {
				continue
			}
			if child, present := v[name]; present {
				errs = append(errs, validateValue(child, sub, joinFieldPath(path, name))...)
			}
		}
		if addl, ok := schema["additionalProperties"].(bool); ok && !addl {
			var unknown []string
			for k := range v {
				if _, known := props[k]; !known {
					unknown = append(unknown, k)
				}
			}
			if len(unknown) > 0 {
				sort.Strings(unknown)
				errs = append(errs, schemaMsg(path, "unexpected properties: "+strings.Join(unknown, ", ")))
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range v {
				errs = append(errs, validateValue(el, items, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	return errs
}

func joinFieldPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func schemaMsg(path, body string) string {
	if path == "" {
		return body
	}
	return path + ": " + body
}

// typeOK reports whether value matches the `type` keyword (string or
// array-of-strings). Malformed or unrecognized type specs are ignored and
// count as a match (forward-compatible).
func typeOK(value any, spec any) bool {
	var names []string
	switch t := spec.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				names = append(names, s)
			}
		}
	default:
		return true
	}
	kind := jsonKind(value)
	checked := false
	for _, n := range names {
		switch n {
		case "object", "array", "string", "boolean", "number":
			checked = true
			if kind == n {
				return true
			}
		case "integer":
			checked = true
			if kind == "number" && integralNumber(value) {
				return true
			}
		}
	}
	return !checked
}

func typeDesc(spec any) string {
	switch t := spec.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " or ")
	default:
		return fmt.Sprintf("%v", spec)
	}
}

// jsonKind names the JSON type of a decoded Go value for error messages.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case float64, int, int64:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func integralNumber(v any) bool {
	switch n := v.(type) {
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0) && n == math.Trunc(n)
	case int, int64:
		return true
	default:
		return false
	}
}

func jsonRepr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// schemaInstructions is the prompt suffix appended to a schema step's
// (already templated) prompt. The schema is echoed compactly; json.Marshal
// sorts map keys, so the suffix is deterministic.
func schemaInstructions(schema map[string]any) string {
	enc, err := json.Marshal(schema)
	if err != nil {
		enc = []byte(fmt.Sprintf("%v", schema))
	}
	return "\n\nRespond with ONLY a single valid JSON value matching this JSON Schema — no prose, no markdown fences:\n" + string(enc)
}

// decodeJSONReply parses a model reply as a single JSON value: it trims
// whitespace, strips a leading ``` or ```json fence and a trailing ```
// when present, then json.Unmarshal's into any.
func decodeJSONReply(reply string) (any, error) {
	s := strings.TrimSpace(reply)
	if strings.HasPrefix(s, "```") {
		s = s[3:]
		if len(s) >= 4 && strings.EqualFold(s[:4], "json") {
			s = s[4:]
		}
		s = strings.TrimSpace(s)
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// checkStructuredReply decodes and validates one model reply against a
// step schema, returning (decoded, problems). problems is empty when the
// reply is a JSON value that matches the schema.
func checkStructuredReply(reply string, schema map[string]any) (any, []string) {
	v, err := decodeJSONReply(reply)
	if err != nil {
		return nil, []string{fmt.Sprintf("reply was not valid JSON: %v", err)}
	}
	return v, validateAgainstSchema(v, schema)
}

// normalizeSchema recursively converts YAML-decoded values into the
// map[string]any / []any shape json.Unmarshal produces, which the
// validator and json.Marshal both expect. gopkg.in/yaml.v3 already yields
// map[string]any for string-keyed mappings (verified in loader_test.go);
// mappings with any non-string key decode as map[any]any, which this
// converts too.
func normalizeSchema(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = normalizeSchema(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[fmt.Sprint(k)] = normalizeSchema(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeSchema(item)
		}
		return out
	default:
		return v
	}
}
