package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"unicode"

	"codeflower-app/internal/agents"
	"codeflower-app/internal/config"
)

// configSchema is the JSON Schema of config.yaml, built from config.Config's
// fields (by their yaml names) and described by their doc comments, so a field
// documented in config.go is documented on the site and in editors.
func configSchema(source string) (map[string]any, error) {
	comments, err := fieldComments(source)
	if err != nil {
		return nil, err
	}
	schema := typeSchema(reflect.TypeOf(config.Config{}), comments)
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = "https://docs.codeflower.app/schema/config.json"
	schema["title"] = "code flower config.yaml"
	// Agents are keyed by their ids; the descriptors list them.
	properties := schema["properties"].(map[string]any)
	properties["agents"].(map[string]any)["propertyNames"] = map[string]any{"enum": agents.IDs()}
	return schema, nil
}

type comments struct {
	types  map[string]string
	fields map[string]map[string]string
}

func fieldComments(source string) (comments, error) {
	found := comments{types: map[string]string{}, fields: map[string]map[string]string{}}
	packages, err := parser.ParseDir(token.NewFileSet(), source, nil, parser.ParseComments)
	if err != nil {
		return found, err
	}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, declaration := range file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.TYPE {
					continue
				}
				for _, spec := range general.Specs {
					typeSpec := spec.(*ast.TypeSpec)
					structType, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						continue
					}
					name := typeSpec.Name.Name
					found.types[name] = clean(name, general.Doc.Text())
					found.fields[name] = map[string]string{}
					for _, field := range structType.Fields.List {
						text := field.Doc.Text()
						if text == "" {
							text = field.Comment.Text()
						}
						for _, fieldName := range field.Names {
							found.fields[name][fieldName.Name] = clean(fieldName.Name, text)
						}
					}
				}
			}
		}
	}
	return found, nil
}

// clean makes a Go doc comment read as a description: one line, without the
// identifier it starts with ("URL is the tunnel" → "The tunnel").
func clean(identifier, text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for _, prefix := range []string{identifier + ": ", identifier + " is ", identifier + " are ", identifier + " "} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			text = rest
			break
		}
	}
	if text == "" {
		return ""
	}
	runes := []rune(text)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func typeSchema(t reflect.Type, docs comments) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		return map[string]any{"type": "integer"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem(), docs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": typeSchema(t.Elem(), docs)}
	case reflect.Struct:
		properties := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if name == "" || name == "-" || !field.IsExported() {
				continue
			}
			property := typeSchema(field.Type, docs)
			if description := docs.fields[t.Name()][field.Name]; description != "" {
				property["description"] = description
			}
			properties[name] = property
		}
		schema := map[string]any{
			"type":       "object",
			"properties": properties,
			// config.yaml refuses unknown keys.
			"additionalProperties": false,
		}
		if description := docs.types[t.Name()]; description != "" {
			schema["description"] = description
		}
		return schema
	}
	return map[string]any{}
}
