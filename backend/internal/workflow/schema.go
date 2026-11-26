package workflow

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Values map[string]json.RawMessage
type Fields map[string]Field

type Field struct {
	Kind       string   `json:"kind"`
	Optional   bool     `json:"optional,omitempty"`
	Nullable   bool     `json:"nullable,omitempty"`
	Items      *Field   `json:"items,omitempty"`
	Properties Fields   `json:"properties,omitempty"`
	Formats    []string `json:"formats,omitempty"`
	Enum       []string `json:"enum,omitempty"`
	MaxItems   int      `json:"max_items,omitempty"`
}

type FileLookup func(context.Context, string) (string, error)

func ValidID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil
}

func (f Field) maxItems() int {
	if f.MaxItems > 0 {
		return f.MaxItems
	}
	return 50
}

func (f Field) accepts(source Field, dynamic bool) bool {
	if source.Optional && !f.Optional || source.Nullable && !f.Nullable {
		return false
	}
	if f.Kind != source.Kind {
		return f.Kind == "number" && source.Kind == "integer"
	}
	switch f.Kind {
	case "file":
		if len(f.Formats) == 0 {
			return true
		}
		if len(source.Formats) == 0 {
			return dynamic
		}
		for _, format := range source.Formats {
			if !slices.Contains(f.Formats, format) {
				return false
			}
		}
	case "string":
		if len(f.Enum) == 0 {
			return true
		}
		if len(source.Enum) == 0 {
			return false
		}
		for _, value := range source.Enum {
			if !slices.Contains(f.Enum, value) {
				return false
			}
		}
	case "array":
		return f.Items != nil && source.Items != nil && (dynamic || source.maxItems() <= f.maxItems()) && f.Items.accepts(*source.Items, dynamic)
	case "object":
		for name, field := range f.Properties {
			provided, ok := source.Properties[name]
			if !ok && !field.Optional || ok && !field.accepts(provided, dynamic) {
				return false
			}
		}
		for name := range source.Properties {
			if _, ok := f.Properties[name]; !ok {
				return false
			}
		}
	}
	return true
}

func (f Field) check(ctx context.Context, lookup FileLookup, value json.RawMessage) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	value = bytes.TrimSpace(value)
	if string(value) == "null" {
		if f.Nullable {
			return "", nil
		}
		return "TYPE_MISMATCH", nil
	}
	if len(value) == 0 {
		return "REQUIRED", nil
	}
	switch f.Kind {
	case "string", "date", "datetime", "file":
		var text string
		if json.Unmarshal(value, &text) != nil {
			return "TYPE_MISMATCH", nil
		}
		switch f.Kind {
		case "string":
			if len(f.Enum) != 0 && !slices.Contains(f.Enum, text) {
				return "INVALID_VALUE", nil
			}
		case "date":
			if _, err := time.Parse(time.DateOnly, text); err != nil {
				return "INVALID_VALUE", nil
			}
		case "datetime":
			if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
				return "INVALID_VALUE", nil
			}
		case "file":
			if !ValidID(text) || lookup == nil {
				return "FILE_UNAVAILABLE", nil
			}
			mime, err := lookup(ctx, text)
			if err != nil {
				return "", err
			}
			if mime == "" {
				return "FILE_UNAVAILABLE", nil
			}
			if len(f.Formats) > 0 && !slices.Contains(f.Formats, mime) {
				return "FORMAT_MISMATCH", nil
			}
		}
	case "integer":
		if _, err := strconv.ParseInt(string(value), 10, 64); err != nil || !json.Valid(value) {
			return "TYPE_MISMATCH", nil
		}
	case "number":
		n, err := strconv.ParseFloat(string(value), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || !json.Valid(value) {
			return "TYPE_MISMATCH", nil
		}
	case "boolean":
		if string(value) != "true" && string(value) != "false" {
			return "TYPE_MISMATCH", nil
		}
	case "array":
		var list []json.RawMessage
		if f.Items == nil || json.Unmarshal(value, &list) != nil {
			return "TYPE_MISMATCH", nil
		}
		if len(list) > f.maxItems() {
			return "LIMIT_EXCEEDED", nil
		}
		for _, item := range list {
			if code, err := f.Items.check(ctx, lookup, item); code != "" || err != nil {
				return code, err
			}
		}
	case "object":
		var obj Values
		if json.Unmarshal(value, &obj) != nil {
			return "TYPE_MISMATCH", nil
		}
		problems, err := checkValues(ctx, lookup, f.Properties, obj, "", "")
		if err != nil {
			return "", err
		}
		if len(problems) > 0 {
			return problems[0].Code, nil
		}
	default:
		return "TYPE_MISMATCH", nil
	}
	return "", nil
}

func checkValues(ctx context.Context, lookup FileLookup, fields Fields, values Values, path, step string) ([]Problem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(values) > 64 {
		return []Problem{{"LIMIT_EXCEEDED", path, step}}, nil
	}
	var problems []Problem
	for name, field := range fields {
		value, ok := values[name]
		if !ok && field.Optional {
			continue
		}
		code, err := field.check(ctx, lookup, value)
		if err != nil {
			return nil, err
		}
		if code != "" {
			problems = append(problems, Problem{code, fieldPath(path, name), step})
		}
	}
	for name := range values {
		if _, ok := fields[name]; !ok {
			problems = append(problems, Problem{"UNKNOWN_FIELD", fieldPath(path, name), step})
		}
	}
	return problems, nil
}

func fieldPath(path, key string) string {
	if len(key) > 64 {
		return path
	}
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
