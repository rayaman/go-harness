package workflow

import (
	"strings"
	"testing"
)

// helpers to keep the table below readable
func obj(v map[string]any) map[string]any { return v }
func arr(v ...any) []any                  { return v }

func TestValidateAgainstSchema(t *testing.T) {
	cases := []struct {
		name   string
		value  any
		schema map[string]any
		want   []string // expected error substrings; empty/nil = must be valid
	}{
		{
			name:  "valid object passes",
			value: obj(map[string]any{"title": "x", "n": 3.0}),
			schema: obj(map[string]any{
				"type":     "object",
				"required": arr("title"),
				"properties": obj(map[string]any{
					"title": obj(map[string]any{"type": "string"}),
					"n":     obj(map[string]any{"type": "integer"}),
				}),
			}),
			want: nil,
		},
		{
			name:   "missing required property",
			value:  obj(map[string]any{}),
			schema: obj(map[string]any{"type": "object", "required": arr("author")}),
			want:   []string{`missing required property "author"`},
		},
		{
			name:  "wrong primitive type",
			value: obj(map[string]any{"title": 42.0}),
			schema: obj(map[string]any{
				"type":       "object",
				"properties": obj(map[string]any{"title": obj(map[string]any{"type": "string"})}),
			}),
			want: []string{"title: expected string, got number"},
		},
		{
			name:  "integer rejects 1.5",
			value: obj(map[string]any{"n": 1.5}),
			schema: obj(map[string]any{
				"type":       "object",
				"properties": obj(map[string]any{"n": obj(map[string]any{"type": "integer"})}),
			}),
			want: []string{"n: expected integer, got number"},
		},
		{
			name:  "integer accepts integral float",
			value: obj(map[string]any{"n": 42.0}),
			schema: obj(map[string]any{
				"type":       "object",
				"properties": obj(map[string]any{"n": obj(map[string]any{"type": "integer"})}),
			}),
			want: nil,
		},
		{
			name:  "nested properties path in error message",
			value: obj(map[string]any{"a": obj(map[string]any{"b": 7.0})}),
			schema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"a": obj(map[string]any{
						"type":       "object",
						"properties": obj(map[string]any{"b": obj(map[string]any{"type": "string"})}),
					}),
				}),
			}),
			want: []string{"a.b: expected string, got number"},
		},
		{
			name:  "items element error path tags[1]",
			value: obj(map[string]any{"tags": arr("ok", 5.0)}),
			schema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"tags": obj(map[string]any{"type": "array", "items": obj(map[string]any{"type": "string"})}),
				}),
			}),
			want: []string{"tags[1]: expected string, got number"},
		},
		{
			name:   "enum membership",
			value:  "blue",
			schema: obj(map[string]any{"enum": arr("red", "green")}),
			want:   []string{`value "blue" not in enum ["red","green"]`},
		},
		{
			name:   "enum accepts member",
			value:  "red",
			schema: obj(map[string]any{"enum": arr("red", "green")}),
			want:   nil,
		},
		{
			name:  "additionalProperties false lists unknown keys",
			value: obj(map[string]any{"a": "x", "c": 1.0, "b": 2.0}),
			schema: obj(map[string]any{
				"type":                 "object",
				"properties":           obj(map[string]any{"a": obj(map[string]any{"type": "string"})}),
				"additionalProperties": false,
			}),
			want: []string{"unexpected properties: b, c"},
		},
		{
			name:   "type array-of-types accepted",
			value:  5.0,
			schema: obj(map[string]any{"type": arr("string", "number")}),
			want:   nil,
		},
		{
			name:   "type array-of-types rejects non-member",
			value:  true,
			schema: obj(map[string]any{"type": arr("string", "number")}),
			want:   []string{"expected string or number, got boolean"},
		},
		{
			name:  "unknown schema keywords ignored",
			value: obj(map[string]any{"title": "x"}),
			schema: obj(map[string]any{
				"type":        "object",
				"$schema":     "http://json-schema.org/draft-07/schema#",
				"title":       "Thing",
				"description": "a thing",
				"minLength":   3,
				"examples":    arr(obj(map[string]any{"title": "e"})),
				"properties":  obj(map[string]any{"title": obj(map[string]any{"type": "string"})}),
			}),
			want: nil,
		},
		{
			name:   "top-level kind mismatch",
			value:  arr(),
			schema: obj(map[string]any{"type": "object"}),
			want:   []string{"expected object, got array"},
		},
		{
			name:  "properties recurse into present keys only",
			value: obj(map[string]any{}),
			schema: obj(map[string]any{
				"type":       "object",
				"properties": obj(map[string]any{"gone": obj(map[string]any{"type": "string"})}),
			}),
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateAgainstSchema(tc.value, tc.schema)
			if len(tc.want) == 0 {
				if len(errs) != 0 {
					t.Fatalf("expected valid, got %v", errs)
				}
				return
			}
			joined := strings.Join(errs, " | ")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Fatalf("errors %q do not contain %q", joined, w)
				}
			}
		})
	}
}

func TestSchemaInstructions(t *testing.T) {
	schema := obj(map[string]any{"type": "object", "required": arr("title")})
	got := schemaInstructions(schema)
	if !strings.HasPrefix(got, "\n\nRespond with ONLY a single valid JSON value matching this JSON Schema — no prose, no markdown fences:\n") {
		t.Errorf("missing prompt prefix: %q", got)
	}
	// Compact, key-sorted JSON echo.
	if !strings.Contains(got, `{"required":["title"],"type":"object"}`) {
		t.Errorf("schema echo not compact/sorted: %q", got)
	}
}

func TestDecodeJSONReply(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantErr bool
	}{
		{"plain json", `{"a": 1}`, false},
		{"whitespace", "  \n {\"a\": 1} \n", false},
		{"json fence", "```json\n{\"a\": 1}\n```", false},
		{"bare fence", "  ```\n{\"a\": 1}\n```  ", false},
		{"prose fails", "Sure! The title is Hello.", true},
		{"empty fails", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := decodeJSONReply(tc.reply)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			m, ok := v.(map[string]any)
			if !ok || m["a"] != 1.0 {
				t.Errorf("decoded wrong: %#v", v)
			}
		})
	}
}
