package openai_compat

import (
	"testing"
)

func TestValidateStrictSchema_ValidObject(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required": []any{"name", "age"},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if !result.Valid {
		t.Errorf("expected valid, got errors: %v", result.Errors)
	}
}

func TestValidateStrictSchema_MissingAdditionalProperties(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"required": []any{"name"},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if result.Valid {
		t.Error("expected invalid due to missing additionalProperties")
	}
}

func TestValidateStrictSchema_AdditionalPropertiesTrue(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": true,
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"required": []any{"name"},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if result.Valid {
		t.Error("expected invalid due to additionalProperties: true")
	}
}

func TestValidateStrictSchema_MissingRequiredProperty(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"email": map[string]any{"type": "string"},
		},
		"required": []any{"name"},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if result.Valid {
		t.Error("expected invalid due to missing required property")
	}
}

func TestValidateStrictSchema_NestedObject(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"config": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"debug": map[string]any{"type": "boolean"},
				},
				"required": []any{"debug"},
			},
		},
		"required": []any{"config"},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if !result.Valid {
		t.Errorf("expected valid nested object, got errors: %v", result.Errors)
	}
}

func TestValidateStrictSchema_NestedObjectMissingAdditionalProps(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"config": map[string]any{
				"type":       "object",
				"properties": map[string]any{
					"debug": map[string]any{"type": "boolean"},
				},
				"required": []any{"debug"},
			},
		},
		"required": []any{"config"},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if result.Valid {
		t.Error("expected invalid due to nested object missing additionalProperties")
	}
}

func TestValidateStrictSchema_ArrayWithItems(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"items": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []any{"items"},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if !result.Valid {
		t.Errorf("expected valid array schema, got errors: %v", result.Errors)
	}
}

func TestSanitizeSchemaForStrictMode(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"nested": map[string]any{
				"type":       "object",
				"properties": map[string]any{
					"val": map[string]any{"type": "integer"},
				},
			},
		},
	}

	sanitized := SanitizeSchemaForStrictMode(schema)

	if ap, ok := sanitized["additionalProperties"].(bool); !ok || ap {
		t.Error("expected additionalProperties: false at root")
	}

	if req, ok := sanitized["required"].([]string); ok {
		found := false
		for _, r := range req {
			if r == "name" {
				found = true
			}
		}
		if !found {
			t.Error("expected 'name' in required array")
		}
	} else {
		t.Error("expected required array to be added")
	}

	if nested, ok := sanitized["properties"].(map[string]any)["nested"].(map[string]any); ok {
		if ap, ok := nested["additionalProperties"].(bool); !ok || ap {
			t.Error("expected additionalProperties: false on nested object")
		}
	}
}

func TestSanitizeSchemaForStrictMode_AlreadyValid(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"required": []any{"name"},
	}

	sanitized := SanitizeSchemaForStrictMode(schema)

	if ap, ok := sanitized["additionalProperties"].(bool); !ok || ap {
		t.Error("expected additionalProperties: false to be preserved")
	}
}

func TestValidateStrictSchema_AnyOf(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "number"},
		},
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if !result.Valid {
		t.Errorf("expected valid anyOf schema, got errors: %v", result.Errors)
	}
}

func TestValidateStrictSchema_UnsupportedNullType(t *testing.T) {
	schema := map[string]any{
		"type": "null",
	}

	result := ValidateStrictSchema(schema, "test_fn")
	if result.Valid {
		t.Error("expected invalid for null type in strict mode")
	}
}
