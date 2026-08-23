package tools

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// validateToolArguments enforces the provider-facing subset of JSON Schema
// used by Sandbar's tool catalog. Keeping this check at the Registry choke
// point means untrusted provider arguments cannot bypass additionalProperties,
// primitive type, enum, or nested item constraints before approval/execution.
func validateToolArguments(schema map[string]interface{}, args map[string]interface{}) error {
	if schema == nil {
		return nil
	}
	return validateSchemaValue(args, schema, "arguments")
}

func validateSchemaValue(value interface{}, schema map[string]interface{}, path string) error {
	if alternatives, ok := schema["anyOf"].([]interface{}); ok && len(alternatives) > 0 {
		matched := false
		for _, raw := range alternatives {
			candidate, ok := raw.(map[string]interface{})
			if ok && validateSchemaValue(value, candidate, path) == nil {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s does not match an allowed form", path)
		}
	}

	if constant, exists := schema["const"]; exists && !schemaValuesEqual(value, constant) {
		return fmt.Errorf("%s must equal %v", path, constant)
	}
	if enum, exists := schema["enum"]; exists && !schemaEnumContains(enum, value) {
		return fmt.Errorf("%s must be one of %v", path, enum)
	}

	typeName, _ := schema["type"].(string)
	switch typeName {
	case "":
		return nil
	case "object":
		object, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		properties, _ := schema["properties"].(map[string]interface{})
		if additional, specified := schema["additionalProperties"].(bool); specified && !additional {
			var unknown []string
			for key := range object {
				if _, known := properties[key]; !known {
					unknown = append(unknown, key)
				}
			}
			if len(unknown) > 0 {
				sort.Strings(unknown)
				return fmt.Errorf("%s contains unsupported fields: %s", path, strings.Join(unknown, ", "))
			}
		}
		for _, required := range requiredFields(schema) {
			if item, exists := object[required]; !exists || item == nil {
				return fmt.Errorf("%s.%s is required", path, required)
			}
		}
		for key, item := range object {
			rawProperty, known := properties[key]
			if !known {
				continue
			}
			property, ok := rawProperty.(map[string]interface{})
			if !ok {
				continue
			}
			if err := validateSchemaValue(item, property, path+"."+key); err != nil {
				return err
			}
		}
		return nil
	case "array":
		items, ok := schemaSlice(value)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		if minimum, ok := schemaInt(schema["minItems"]); ok && len(items) < minimum {
			return fmt.Errorf("%s must contain at least %d item(s)", path, minimum)
		}
		if rawItemSchema, ok := schema["items"].(map[string]interface{}); ok {
			for i, item := range items {
				if err := validateSchemaValue(item, rawItemSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		return nil
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", path)
		}
		if minimum, ok := schemaInt(schema["minLength"]); ok && len(text) < minimum {
			return fmt.Errorf("%s must contain at least %d character(s)", path, minimum)
		}
		if pattern, ok := schema["pattern"].(string); ok {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("%s has invalid schema pattern: %w", path, err)
			}
			if !re.MatchString(text) {
				return fmt.Errorf("%s has an invalid format", path)
			}
		}
		return nil
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
		return nil
	case "integer":
		number, ok := schemaNumber(value)
		if !ok || math.Trunc(number) != number {
			return fmt.Errorf("%s must be an integer", path)
		}
		return validateNumberBounds(number, schema, path)
	case "number":
		number, ok := schemaNumber(value)
		if !ok {
			return fmt.Errorf("%s must be a number", path)
		}
		return validateNumberBounds(number, schema, path)
	default:
		return fmt.Errorf("%s uses unsupported schema type %q", path, typeName)
	}
}

func schemaSlice(value interface{}) ([]interface{}, bool) {
	switch value := value.(type) {
	case []interface{}:
		return value, true
	case []string:
		items := make([]interface{}, len(value))
		for i := range value {
			items[i] = value[i]
		}
		return items, true
	default:
		return nil, false
	}
}

func schemaNumber(value interface{}) (float64, bool) {
	switch value := value.(type) {
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	case float32:
		return float64(value), true
	case float64:
		return value, true
	default:
		return 0, false
	}
}

func schemaInt(value interface{}) (int, bool) {
	number, ok := schemaNumber(value)
	if !ok || math.Trunc(number) != number {
		return 0, false
	}
	return int(number), true
}

func validateNumberBounds(number float64, schema map[string]interface{}, path string) error {
	if minimum, ok := schemaNumber(schema["minimum"]); ok && number < minimum {
		return fmt.Errorf("%s must be at least %v", path, minimum)
	}
	if maximum, ok := schemaNumber(schema["maximum"]); ok && number > maximum {
		return fmt.Errorf("%s must be at most %v", path, maximum)
	}
	if minimum, ok := schemaNumber(schema["exclusiveMinimum"]); ok && number <= minimum {
		return fmt.Errorf("%s must be greater than %v", path, minimum)
	}
	return nil
}

func schemaEnumContains(raw, value interface{}) bool {
	switch candidates := raw.(type) {
	case []string:
		candidate, ok := value.(string)
		if !ok {
			return false
		}
		for _, allowed := range candidates {
			if candidate == allowed {
				return true
			}
		}
	case []interface{}:
		for _, allowed := range candidates {
			if schemaValuesEqual(value, allowed) {
				return true
			}
		}
	}
	return false
}

func schemaValuesEqual(left, right interface{}) bool {
	if leftNumber, ok := schemaNumber(left); ok {
		if rightNumber, rightOK := schemaNumber(right); rightOK {
			return leftNumber == rightNumber
		}
	}
	return reflect.DeepEqual(left, right)
}
