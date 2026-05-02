package openai_compat

import (
	"fmt"
	"strings"
)

// StrictSchemaValidationResult contains the results of validating a tool's
// parameter schema against DeepSeek V4 strict mode requirements.
type StrictSchemaValidationResult struct {
	Valid    bool
	Errors   []string
	Warnings []string
}

// ValidateStrictSchema validates that a JSON Schema conforms to DeepSeek V4's
// strict mode requirements. Strict mode guarantees that tool call outputs
// conform exactly to the schema, but requires:
//   - All object properties must be listed in the "required" array
//   - "additionalProperties: false" must be set on every object
//   - Only supported types are used: object, string, number, integer,
//     boolean, array, enum (via enum keyword), anyOf
func ValidateStrictSchema(schema map[string]any, functionName string) StrictSchemaValidationResult {
	result := StrictSchemaValidationResult{Valid: true}
	validateStrictSchemaRecursive(schema, functionName, "", &result)
	if len(result.Errors) > 0 {
		result.Valid = false
	}
	return result
}

func validateStrictSchemaRecursive(schema map[string]any, fnName, path string, result *StrictSchemaValidationResult) {
	if schema == nil {
		return
	}
	schemaType, _ := schema["type"].(string)
	switch schemaType {
	case "null":
		result.Errors = append(result.Errors,
			fmt.Sprintf("function %q: type %q at %q is not supported in strict mode", fnName, schemaType, path))
		return
	case "":
		if _, hasAnyOf := schema["anyOf"]; hasAnyOf {
			validateStrictAnyOf(schema, fnName, path, result)
			return
		}
		if _, hasOneOf := schema["oneOf"]; hasOneOf {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("function %q: oneOf at %q is not explicitly supported in strict mode; consider using anyOf", fnName, path))
			return
		}
		if _, hasAllOf := schema["allOf"]; hasAllOf {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("function %q: allOf at %q is not explicitly supported in strict mode", fnName, path))
			return
		}
		return
	}
	if schemaType == "object" {
		validateStrictObject(schema, fnName, path, result)
	}
	if schemaType == "array" {
		validateStrictArray(schema, fnName, path, result)
	}
}

func validateStrictObject(schema map[string]any, fnName, path string, result *StrictSchemaValidationResult) {
	objPath := path
	if objPath == "" {
		objPath = "(root)"
	}
	additionalProps, hasAdditionalProps := schema["additionalProperties"]
	if !hasAdditionalProps {
		result.Errors = append(result.Errors,
			fmt.Sprintf("function %q: object at %q missing \"additionalProperties: false\" (required for strict mode)", fnName, objPath))
	} else if boolVal, ok := additionalProps.(bool); !ok || boolVal {
		result.Errors = append(result.Errors,
			fmt.Sprintf("function %q: object at %q has \"additionalProperties\" not set to false (required for strict mode)", fnName, objPath))
	}
	properties, hasProperties := schema["properties"].(map[string]any)
	if hasProperties && len(properties) > 0 {
		requiredSlice, _ := schema["required"].([]any)
		requiredSet := make(map[string]bool, len(requiredSlice))
		for _, r := range requiredSlice {
			if name, ok := r.(string); ok {
				requiredSet[name] = true
			}
		}
		for propName := range properties {
			if !requiredSet[propName] {
				result.Errors = append(result.Errors,
					fmt.Sprintf("function %q: property %q at %q is missing from \"required\" array (all properties must be required in strict mode)", fnName, propName, objPath))
			}
		}
		for propName, propSchema := range properties {
			if nestedSchema, ok := propSchema.(map[string]any); ok {
				nestedPath := objPath + "." + propName
				validateStrictSchemaRecursive(nestedSchema, fnName, nestedPath, result)
			}
		}
	}
}

func validateStrictArray(schema map[string]any, fnName, path string, result *StrictSchemaValidationResult) {
	arrPath := path
	if arrPath == "" {
		arrPath = "(root)"
	}
	if items, ok := schema["items"].(map[string]any); ok {
		validateStrictSchemaRecursive(items, fnName, arrPath+"[]", result)
	}
}

func validateStrictAnyOf(schema map[string]any, fnName, path string, result *StrictSchemaValidationResult) {
	anyOfSlice, ok := schema["anyOf"].([]any)
	if !ok {
		return
	}
	for i, item := range anyOfSlice {
		if itemSchema, ok := item.(map[string]any); ok {
			validateStrictSchemaRecursive(itemSchema, fnName,
				fmt.Sprintf("%s[anyOf:%d]", path, i), result)
		}
	}
}

// SanitizeSchemaForStrictMode attempts to fix common schema issues that would
// prevent strict mode from working. It adds missing "required" arrays and
// "additionalProperties: false" to objects.
func SanitizeSchemaForStrictMode(schema map[string]any) map[string]any {
	if schema == nil {
		return schema
	}
	result := make(map[string]any, len(schema))
	for k, v := range schema {
		result[k] = v
	}
	schemaType, _ := schema["type"].(string)
	if schemaType == "object" {
		if _, hasAP := result["additionalProperties"]; !hasAP {
			result["additionalProperties"] = false
		}
		if properties, ok := result["properties"].(map[string]any); ok && len(properties) > 0 {
			_, hasRequired := result["required"]
			if !hasRequired {
				required := make([]string, 0, len(properties))
				for propName := range properties {
					required = append(required, propName)
				}
				result["required"] = required
			}
			newProps := make(map[string]any, len(properties))
			for propName, propSchema := range properties {
				if nestedSchema, ok := propSchema.(map[string]any); ok {
					newProps[propName] = SanitizeSchemaForStrictMode(nestedSchema)
				} else {
					newProps[propName] = propSchema
				}
			}
			result["properties"] = newProps
		}
	}
	if schemaType == "array" {
		if items, ok := result["items"].(map[string]any); ok {
			result["items"] = SanitizeSchemaForStrictMode(items)
		}
	}
	return result
}

// FormatValidationResult returns a human-readable string from a validation result.
func FormatValidationResult(result StrictSchemaValidationResult) string {
	var sb strings.Builder
	if result.Valid {
		sb.WriteString("Schema is valid for strict mode")
	} else {
		sb.WriteString("Schema is NOT valid for strict mode:\n")
		for _, e := range result.Errors {
			sb.WriteString("  ERROR: ")
			sb.WriteString(e)
			sb.WriteString("\n")
		}
	}
	for _, w := range result.Warnings {
		sb.WriteString("  WARNING: ")
		sb.WriteString(w)
		sb.WriteString("\n")
	}
	return sb.String()
}
