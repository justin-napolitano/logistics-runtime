package httpapi

import (
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/shopping"
)

type operation struct {
	Method, Path, Summary string
	Request               any
	Handle                func(*API, http.ResponseWriter, *http.Request)
}

// This registry is used both to register routes and generate OpenAPI paths.
var operations = []operation{
	{"GET", "/v1/items", "List inventory; view=all|unknown|shortages, category, limit and offset", nil, (*API).list},
	{"POST", "/v1/items", "Create an item with a stable caller-supplied ID", CreateRequest{}, (*API).create},
	{"GET", "/v1/items/{id}", "Read an item and its derived gap", nil, (*API).get},
	{"PUT", "/v1/items/{id}", "Replace all editable fields conditional on revision", ReplaceRequest{}, (*API).replace},
}

// Spec derives request/response schemas from the executable Go JSON types.
func Spec() map[string]any {
	paths := map[string]any{}
	for _, op := range operations {
		path, ok := paths[op.Path].(map[string]any)
		if !ok {
			path = map[string]any{}
			paths[op.Path] = path
		}
		response := schema(reflect.TypeOf(inventory.View{}))
		if op.Path == "/v1/items" && op.Method == "GET" {
			response = map[string]any{"type": "object", "properties": map[string]any{"items": map[string]any{"type": "array", "items": response}, "limit": map[string]any{"type": "integer"}, "offset": map[string]any{"type": "integer"}}}
		}
		status := "200"
		if op.Method == "POST" {
			status = "201"
		}
		entry := map[string]any{"summary": op.Summary, "security": []any{map[string]any{"bearerAuth": []string{}}}, "responses": map[string]any{status: map[string]any{"description": "Success", "content": map[string]any{"application/json": map[string]any{"schema": response}}}, "default": map[string]any{"description": "JSON error; 400 malformed input, 401 unauthorized, 404 missing, 409 conflict, 415 media type, 422 invalid values, 500 unavailable"}}}
		if strings.Contains(op.Path, "{id}") {
			entry["parameters"] = []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9][a-zA-Z0-9._-]{0,119}$"}}}
		}
		if op.Method == "GET" && op.Path == "/v1/items" {
			entry["parameters"] = []any{
				map[string]any{"name": "view", "in": "query", "schema": map[string]any{"type": "string", "enum": []string{"all", "unknown", "shortages"}}},
				map[string]any{"name": "category", "in": "query", "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 100}},
				map[string]any{"name": "offset", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000, "default": 0}},
			}
		}
		if op.Request != nil {
			entry["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema(reflect.TypeOf(op.Request))}}}
		}
		path[strings.ToLower(op.Method)] = entry
	}
	shoppingSpec(paths)
	return map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "Logistics inventory, shopping and email reports API", "version": "0.2.0"}, "paths": paths, "components": map[string]any{"securitySchemes": map[string]any{"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"}}}}
}

func schema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{schema(t.Elem()), map[string]any{"type": "null"}}}
	}
	if t == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schema(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int64, reflect.Int:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Struct:
		props := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				for k, v := range schema(f.Type)["properties"].(map[string]any) {
					props[k] = v
				}
				continue
			}
			props[strings.Split(f.Tag.Get("json"), ",")[0]] = schema(f.Type)
		}
		out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if _, ok := props["name"]; ok {
			out["required"] = []string{"name"}
		}
		if t == reflect.TypeOf(CreateRequest{}) {
			out["required"] = []string{"id", "name"}
		}
		if t == reflect.TypeOf(ReplaceRequest{}) {
			out["required"] = []string{"revision", "name"}
		}
		if t == reflect.TypeOf(shopping.Template{}) {
			out["required"] = []string{"name", "lines"}
		}
		if t == reflect.TypeOf(shopping.Line{}) {
			out["required"] = []string{"id", "name", "merchant", "package", "basis"}
		}
		if t == reflect.TypeOf(BasketRequest{}) {
			out["required"] = []string{"template_id", "cycle"}
		}
		return out
	default:
		panic("unsupported API schema type")
	}
}
