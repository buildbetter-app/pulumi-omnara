package gen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	pschema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"

	"github.com/buildbetter-app/pulumi-omnara/provider/pkg/meta"
)

const anyRef = "pulumi.json#/Any"

// secretFieldNames are API fields that always hold credentials.
var secretFieldNames = map[string]bool{
	"token": true, "material": true, "password": true, "client_secret": true,
	"private_key": true, "secret_access_key": true, "session_token": true,
	"access_token": true, "refresh_token": true, "signing_secret": true,
}

// typeBuilder converts OpenAPI schemas into Pulumi types and runtime shapes.
type typeBuilder struct {
	spec     *Spec
	pkg      string
	types    map[string]pschema.ComplexTypeSpec
	shapes   map[string]*meta.Object
	building map[string]bool
	// componentTokens maps component schema names to type tokens.
	componentTokens map[string]string
	// inline maps a structural key of an inline object to its token.
	inline map[string]string
	// reserved holds names that types must not take (resource names).
	reserved map[string]bool
	used     map[string]bool
}

func newTypeBuilder(spec *Spec, pkg string, reserved map[string]bool) *typeBuilder {
	return &typeBuilder{
		spec: spec, pkg: pkg,
		types:           map[string]pschema.ComplexTypeSpec{},
		shapes:          map[string]*meta.Object{},
		building:        map[string]bool{},
		componentTokens: map[string]string{},
		inline:          map[string]string{},
		reserved:        reserved,
		used:            map[string]bool{},
	}
}

func (b *typeBuilder) token(name string) string { return b.pkg + ":index:" + name }

// claim reserves a unique type name derived from want.
func (b *typeBuilder) claim(want string) string {
	name := pascal(want)
	if b.reserved[name] {
		name += "Details"
	}
	base := name
	for i := 2; b.used[name]; i++ {
		name = fmt.Sprintf("%s%d", base, i)
	}
	b.used[name] = true
	return name
}

// typed is the result of mapping one schema.
type typed struct {
	spec     pschema.TypeSpec
	shape    *meta.Shape
	nullable bool
	// binary marks string/binary request fields (file uploads).
	binary bool
}

var (
	anyType  = typed{spec: pschema.TypeSpec{Ref: anyRef}, shape: &meta.Shape{Kind: meta.KindAny}}
	primPrim = &meta.Shape{Kind: meta.KindPrimitive}
)

// typeOf maps a schema; ctx names any inline object types it creates.
func (b *typeBuilder) typeOf(n any, ctx string) typed {
	m, ok := n.(node)
	if !ok || len(m) == 0 {
		return anyType
	}
	if ref, ok := m["$ref"].(string); ok {
		name := refName(ref)
		target, _ := b.spec.Schema(name)
		if b.isObject(target) {
			tok := b.component(name, target)
			return typed{spec: pschema.TypeSpec{Ref: "#/types/" + tok}, shape: &meta.Shape{Kind: meta.KindRef, Ref: tok}}
		}
		return b.typeOf(target, name)
	}

	nullable := m["nullable"] == true
	types := typeList(m["type"])
	if i := indexOf(types, "null"); i >= 0 {
		nullable = true
		types = append(types[:i:i], types[i+1:]...)
	}

	for _, key := range []string{"oneOf", "anyOf"} {
		vs, ok := m[key].([]any)
		if !ok || m["properties"] != nil {
			continue
		}
		var variants []any
		for _, v := range vs {
			if isNullSchema(v) {
				nullable = true
				continue
			}
			variants = append(variants, v)
		}
		t := b.union(variants, m["discriminator"], ctx)
		t.nullable = t.nullable || nullable
		return t
	}

	if all, ok := m["allOf"].([]any); ok && m["properties"] == nil && len(all) == 1 {
		t := b.typeOf(all[0], ctx)
		t.nullable = t.nullable || nullable
		return t
	}
	if b.isObject(m) {
		tok := b.inlineObject(ctx, m)
		return typed{spec: pschema.TypeSpec{Ref: "#/types/" + tok}, shape: &meta.Shape{Kind: meta.KindRef, Ref: tok}, nullable: nullable}
	}

	t := typed{nullable: nullable, shape: primPrim}
	kind := ""
	if len(types) > 0 {
		kind = types[0]
	} else if c, ok := m["const"]; ok {
		kind = jsonKind(c)
	} else if _, ok := m["enum"]; ok {
		kind = "string"
	}
	switch kind {
	case "string":
		t.spec = pschema.TypeSpec{Type: "string"}
		t.binary = m["format"] == "binary"
	case "integer", "number", "boolean":
		t.spec = pschema.TypeSpec{Type: kind}
	case "array":
		items := b.typeOf(m["items"], ctx+"Item")
		t.spec = pschema.TypeSpec{Type: "array", Items: &items.spec}
		t.shape = &meta.Shape{Kind: meta.KindArray, Items: items.shape}
	case "object":
		ap := m["additionalProperties"]
		if apm, ok := ap.(node); ok && len(apm) > 0 {
			v := b.typeOf(apm, ctx+"Value")
			t.spec = pschema.TypeSpec{Type: "object", AdditionalProperties: &v.spec}
			t.shape = &meta.Shape{Kind: meta.KindMap, Items: v.shape}
		} else if ap == true || (ap != false && ap != nil) {
			t.spec = pschema.TypeSpec{Type: "object", AdditionalProperties: &pschema.TypeSpec{Ref: anyRef}}
			t.shape = &meta.Shape{Kind: meta.KindMap, Items: &meta.Shape{Kind: meta.KindAny}}
		} else {
			return typed{spec: anyType.spec, shape: anyType.shape, nullable: nullable}
		}
	default:
		return typed{spec: anyType.spec, shape: anyType.shape, nullable: nullable}
	}
	return t
}

// nullable reports whether a schema admits null, without creating types.
func (b *typeBuilder) nullable(n any) bool {
	m, ok := n.(node)
	if !ok {
		return false
	}
	if _, ok := m["$ref"]; ok {
		t, _ := b.spec.Deref(m)
		return b.nullable(t)
	}
	if m["nullable"] == true || indexOf(typeList(m["type"]), "null") >= 0 {
		return true
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		vs, _ := m[key].([]any)
		for _, v := range vs {
			if isNullSchema(v) || b.nullable(v) {
				return true
			}
		}
	}
	if all, ok := m["allOf"].([]any); ok && len(all) == 1 {
		return b.nullable(all[0])
	}
	return false
}

// union maps oneOf/anyOf variants.
func (b *typeBuilder) union(variants []any, disc any, ctx string) typed {
	switch len(variants) {
	case 0:
		return anyType
	case 1:
		return b.typeOf(variants[0], ctx)
	}
	var mapped []typed
	prims := map[string]bool{}
	for i, v := range variants {
		t := b.typeOf(v, fmt.Sprintf("%sVariant%d", ctx, i+1))
		if t.shape.Kind == meta.KindAny {
			return anyType
		}
		mapped = append(mapped, t)
		if t.shape.Kind == meta.KindPrimitive {
			prims[t.spec.Type] = true
		}
	}
	// Variants that differ only by constraints (const 0 | integer) collapse.
	if len(prims) == 1 && len(mapped) == countKind(mapped, meta.KindPrimitive) {
		return mapped[0]
	}
	if len(prims) == 2 && prims["integer"] && prims["number"] && len(mapped) == countKind(mapped, meta.KindPrimitive) {
		return typed{spec: pschema.TypeSpec{Type: "number"}, shape: primPrim}
	}
	out := typed{shape: &meta.Shape{Kind: meta.KindUnion}}
	seen := map[string]bool{}
	for _, t := range mapped {
		key := t.spec.Type + t.spec.Ref
		if t.spec.Items != nil || t.spec.AdditionalProperties != nil {
			// Unions over arrays or maps have no SDK representation.
			return anyType
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out.spec.OneOf = append(out.spec.OneOf, t.spec)
		if t.shape.Kind == meta.KindRef {
			out.shape.Variants = append(out.shape.Variants, t.shape.Ref)
		}
	}
	if d, ok := disc.(node); ok {
		prop, _ := d["propertyName"].(string)
		ds := &pschema.DiscriminatorSpec{PropertyName: camel(prop), Mapping: map[string]string{}}
		mapping, _ := d["mapping"].(node)
		for _, value := range sortedKeys(mapping) {
			r, _ := mapping[value].(string)
			name := refName(r)
			target, _ := b.spec.Schema(name)
			if b.isObject(target) {
				ds.Mapping[value] = "#/types/" + b.component(name, target)
			}
		}
		if len(ds.Mapping) > 0 && len(out.shape.Variants) == len(out.spec.OneOf) {
			out.spec.Discriminator = ds
		}
	}
	if len(out.spec.OneOf) == 1 {
		return typed{spec: out.spec.OneOf[0], shape: shapeFor(out.spec.OneOf[0])}
	}
	return out
}

func shapeFor(t pschema.TypeSpec) *meta.Shape {
	if strings.HasPrefix(t.Ref, "#/types/") {
		return &meta.Shape{Kind: meta.KindRef, Ref: strings.TrimPrefix(t.Ref, "#/types/")}
	}
	return primPrim
}

func countKind(ts []typed, kind string) int {
	n := 0
	for _, t := range ts {
		if t.shape.Kind == kind {
			n++
		}
	}
	return n
}

// isObject reports whether a schema describes an object with named fields.
func (b *typeBuilder) isObject(m node) bool {
	if m == nil {
		return false
	}
	if _, ok := m["$ref"]; ok {
		t, _ := b.spec.Deref(m)
		return b.isObject(t)
	}
	if props, ok := m["properties"].(node); ok && len(props) > 0 {
		return true
	}
	if all, ok := m["allOf"].([]any); ok {
		for _, a := range all {
			if am, ok := a.(node); ok && b.isObject(am) {
				return true
			}
		}
	}
	return false
}

// objectFields flattens properties, allOf, and object-valued oneOf/anyOf
// members into one field list. Fields from oneOf/anyOf variants are optional.
func (b *typeBuilder) objectFields(n any) (map[string]any, map[string]bool) {
	fields := map[string]any{}
	required := map[string]bool{}
	var walk func(n any, optional bool)
	walk = func(n any, optional bool) {
		m, ok := n.(node)
		if !ok {
			return
		}
		if _, ok := m["$ref"]; ok {
			t, _ := b.spec.Deref(m)
			walk(t, optional)
			return
		}
		props, _ := m["properties"].(node)
		for k, v := range props {
			if _, seen := fields[k]; !seen {
				fields[k] = v
			}
		}
		if !optional {
			reqs, _ := m["required"].([]any)
			for _, r := range reqs {
				if s, ok := r.(string); ok {
					required[s] = true
				}
			}
		}
		if all, ok := m["allOf"].([]any); ok {
			for _, a := range all {
				walk(a, optional)
			}
		}
		for _, key := range []string{"oneOf", "anyOf"} {
			if vs, ok := m[key].([]any); ok {
				for _, v := range vs {
					walk(v, true)
				}
			}
		}
	}
	walk(n, false)
	for k := range required {
		if _, ok := fields[k]; !ok {
			delete(required, k)
		}
	}
	return fields, required
}

func (b *typeBuilder) component(name string, m node) string {
	if tok, ok := b.componentTokens[name]; ok {
		return tok
	}
	tok := b.token(b.claim(name))
	b.componentTokens[name] = tok
	b.object(tok, name, m)
	return tok
}

func (b *typeBuilder) inlineObject(ctx string, m node) string {
	key := structuralKey(m)
	if tok, ok := b.inline[key]; ok {
		return tok
	}
	tok := b.token(b.claim(ctx))
	b.inline[key] = tok
	b.object(tok, ctx, m)
	return tok
}

// object emits a Pulumi object type and its runtime field mapping.
func (b *typeBuilder) object(tok, ctx string, m node) {
	if b.building[tok] {
		return
	}
	b.building[tok] = true
	fields, required := b.objectFields(m)
	spec := pschema.ObjectTypeSpec{Type: "object", Description: describe(m), Properties: map[string]pschema.PropertySpec{}}
	shape := &meta.Object{Props: map[string]*meta.Field{}}
	b.shapes[tok] = shape
	for _, api := range sortedKeys(fields) {
		f, _ := fields[api].(node)
		if hidden(f) {
			continue
		}
		name := camel(api)
		t := b.typeOf(f, strings.TrimSuffix(ctx, "Request")+pascal(api))
		ps := pschema.PropertySpec{TypeSpec: t.spec, Description: b.describeField(f)}
		if c, ok := f["const"]; ok {
			ps.Const = c
		}
		if d, ok := f["default"]; ok && isScalar(d) && t.shape.Kind == meta.KindPrimitive {
			ps.Default = d
		}
		if secretFieldNames[api] || f["writeOnly"] == true {
			ps.Secret = true
		}
		spec.Properties[name] = ps
		shape.Props[api] = &meta.Field{Name: name, Shape: t.shape}
		if required[api] && !t.nullable {
			spec.Required = append(spec.Required, name)
		}
	}
	sort.Strings(spec.Required)
	b.types[tok] = pschema.ComplexTypeSpec{ObjectTypeSpec: spec}
}

// describeField prefers the field's own description, then its target's,
// and lists the allowed values of enum fields.
func (b *typeBuilder) describeField(f node) string {
	d := b.fieldDescription(f)
	values := b.enumValues(f)
	if len(values) < 2 {
		return d
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "`" + v + "`"
	}
	note := "Allowed values: " + strings.Join(quoted, ", ") + "."
	if d == "" {
		return note
	}
	return strings.TrimRight(d, " ") + " " + note
}

// enumValues returns the enum of a field, looking through refs and
// nullable wrappers.
func (b *typeBuilder) enumValues(f node) []string {
	m := f
	for range 8 {
		if _, ok := m["$ref"]; ok {
			m, _ = b.spec.Deref(m)
			continue
		}
		var next node
		for _, key := range []string{"oneOf", "anyOf", "allOf"} {
			vs, _ := m[key].([]any)
			var live []node
			for _, v := range vs {
				if vm, ok := v.(node); ok && !isNullSchema(vm) {
					live = append(live, vm)
				}
			}
			if len(live) == 1 {
				next = live[0]
			}
		}
		if next == nil {
			break
		}
		m = next
	}
	var out []string
	enum, _ := m["enum"].([]any)
	for _, e := range enum {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (b *typeBuilder) fieldDescription(f node) string {
	if d := describe(f); d != "" {
		return d
	}
	if _, ok := f["$ref"]; ok {
		t, _ := b.spec.Deref(f)
		return describe(t)
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if vs, ok := f[key].([]any); ok {
			for _, v := range vs {
				if vm, ok := v.(node); ok && !isNullSchema(vm) {
					if d := b.fieldDescription(vm); d != "" {
						return d
					}
				}
			}
		}
	}
	return ""
}

func describe(m node) string {
	d, _ := m["description"].(string)
	if d == "" {
		d, _ = m["title"].(string)
	}
	return strings.TrimSpace(d)
}

func hidden(m node) bool { return m["x-hidden"] == true }

func isNullSchema(v any) bool {
	m, ok := v.(node)
	if !ok {
		return false
	}
	t := typeList(m["type"])
	return len(t) == 1 && t[0] == "null"
}

func typeList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int64, float64:
		return "number"
	}
	return ""
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, int, int64, float64:
		return true
	}
	return false
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// structuralKey identifies an inline schema by structure. Documentation
// differences don't make two inline objects distinct types.
func structuralKey(m node) string {
	b, _ := json.Marshal(stripDocs(m))
	return string(b)
}

func stripDocs(v any) any {
	switch x := v.(type) {
	case node:
		out := make(node, len(x))
		for k, e := range x {
			switch k {
			case "description", "title", "examples", "example":
				continue
			}
			out[k] = stripDocs(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = stripDocs(e)
		}
		return out
	}
	return v
}

// camel converts snake_case API fields and xxxID path parameters to the
// lowerCamel names Pulumi schemas use.
func camel(s string) string {
	if !strings.Contains(s, "_") && strings.HasSuffix(s, "ID") && len(s) > 2 {
		return s[:len(s)-2] + "Id"
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	for i, p := range parts {
		if i == 0 {
			parts[i] = lowerFirst(p)
		} else {
			parts[i] = upperFirst(p)
		}
	}
	return strings.Join(parts, "")
}

func pascal(s string) string { return upperFirst(camel(s)) }

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
