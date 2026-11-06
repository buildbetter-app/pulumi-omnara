package gen

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// node is a decoded YAML/JSON value from the OpenAPI document.
type node = map[string]any

// Spec is a loaded OpenAPI 3.1 document. The generator walks it as plain
// maps because OpenAPI 3.1 features (type arrays, const) are not handled by
// the common Go OpenAPI libraries.
type Spec struct {
	Version string
	schemas node
	params  node
	ops     map[string]*Op
}

// Op is one operation in the document.
type Op struct {
	ID          string
	Method      string
	Path        string
	Summary     string
	Description string
	raw         node
	pathItem    node
}

// Param is a resolved operation parameter.
type Param struct {
	Name        string
	In          string
	Required    bool
	Description string
	Schema      any
}

var pathParamRE = regexp.MustCompile(`\{([^}]+)\}`)

// LoadSpec reads an OpenAPI document.
func LoadSpec(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s := &Spec{ops: map[string]*Op{}}
	if info, ok := root["info"].(node); ok {
		s.Version, _ = info["version"].(string)
	}
	comps, _ := root["components"].(node)
	s.schemas, _ = comps["schemas"].(node)
	s.params, _ = comps["parameters"].(node)
	paths, _ := root["paths"].(node)
	for p, v := range paths {
		item, _ := v.(node)
		for _, m := range []string{"get", "post", "put", "patch", "delete"} {
			raw, ok := item[m].(node)
			if !ok {
				continue
			}
			id, _ := raw["operationId"].(string)
			if id == "" {
				continue
			}
			op := &Op{ID: id, Method: strings.ToUpper(m), Path: p, raw: raw, pathItem: item}
			op.Summary, _ = raw["summary"].(string)
			op.Description, _ = raw["description"].(string)
			s.ops[id] = op
		}
	}
	return s, nil
}

// Op returns the operation with the given operationId.
func (s *Spec) Op(id string) (*Op, error) {
	op, ok := s.ops[id]
	if !ok {
		return nil, fmt.Errorf("operation %q not found in spec", id)
	}
	if !op.AcceptsBearer() {
		return nil, fmt.Errorf("operation %q does not accept bearer tokens", id)
	}
	return op, nil
}

// AcceptsBearer reports whether the operation accepts the bearer tokens
// (personal access tokens and org API keys) the provider authenticates with.
func (o *Op) AcceptsBearer() bool {
	sec, ok := o.raw["security"].([]any)
	if !ok {
		return true // the document-level default includes bearerAuth
	}
	for _, req := range sec {
		if r, ok := req.(node); ok {
			if _, ok := r["bearerAuth"]; ok {
				return true
			}
		}
	}
	return false
}

// PathParams returns the path's parameter names in order.
func (o *Op) PathParams() []string {
	var out []string
	for _, m := range pathParamRE.FindAllStringSubmatch(o.Path, -1) {
		out = append(out, m[1])
	}
	return out
}

// Params returns the operation's parameters, including path-level ones.
func (s *Spec) Params(o *Op) []Param {
	var out []Param
	seen := map[string]bool{}
	add := func(list any) {
		items, _ := list.([]any)
		for _, it := range items {
			p, _ := it.(node)
			if ref, ok := p["$ref"].(string); ok {
				p, _ = s.params[refName(ref)].(node)
			}
			if p == nil {
				continue
			}
			name, _ := p["name"].(string)
			in, _ := p["in"].(string)
			key := in + ":" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			req, _ := p["required"].(bool)
			desc, _ := p["description"].(string)
			out = append(out, Param{Name: name, In: in, Required: req, Description: desc, Schema: p["schema"]})
		}
	}
	add(o.raw["parameters"])
	add(o.pathItem["parameters"])
	return out
}

// Param returns one named parameter of the operation.
func (s *Spec) Param(o *Op, name string) (Param, bool) {
	for _, p := range s.Params(o) {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// RequestBody returns the request schema and its encoding ("json",
// "multipart", or "raw").
func (s *Spec) RequestBody(o *Op) (any, string) {
	rb, _ := o.raw["requestBody"].(node)
	content, _ := rb["content"].(node)
	keys := sortedKeys(content)
	for _, ct := range keys {
		media, _ := content[ct].(node)
		switch {
		case strings.HasPrefix(ct, "application/json"):
			return media["schema"], "json"
		case strings.HasPrefix(ct, "multipart/form-data"):
			return media["schema"], "multipart"
		}
	}
	for _, ct := range keys {
		media, _ := content[ct].(node)
		return media["schema"], "raw"
	}
	return nil, ""
}

// Response returns the JSON schema of the operation's success response,
// preferring 201 over 200.
func (s *Spec) Response(o *Op) any {
	responses, _ := o.raw["responses"].(node)
	for _, code := range []string{"201", "200", "202"} {
		r, ok := responses[code].(node)
		if !ok {
			continue
		}
		content, _ := r["content"].(node)
		for _, ct := range sortedKeys(content) {
			if strings.HasPrefix(ct, "application/json") {
				media, _ := content[ct].(node)
				return media["schema"]
			}
		}
	}
	return nil
}

// Schema returns a component schema by name.
func (s *Spec) Schema(name string) (node, bool) {
	n, ok := s.schemas[name].(node)
	return n, ok
}

// Deref follows $ref chains and returns the target and its component name.
func (s *Spec) Deref(n any) (node, string) {
	name := ""
	m, _ := n.(node)
	for i := 0; i < 32 && m != nil; i++ {
		ref, ok := m["$ref"].(string)
		if !ok {
			break
		}
		name = refName(ref)
		m, _ = s.schemas[name].(node)
	}
	return m, name
}

func refName(ref string) string {
	return ref[strings.LastIndex(ref, "/")+1:]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
