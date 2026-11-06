// Package gen turns the Omnara OpenAPI document and resources.yaml into a
// Pulumi package schema and the runtime metadata the provider embeds.
package gen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	pschema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"

	"github.com/buildbetter-app/pulumi-omnara/provider/pkg/meta"
)

// Result is the generated output.
type Result struct {
	Schema   pschema.PackageSpec
	Metadata meta.Metadata
	Warnings []string
}

type generator struct {
	spec    *Spec
	overlay *Overlay
	types   *typeBuilder
	res     *Result
}

// Generate builds the schema and metadata.
func Generate(spec *Spec, overlay *Overlay, version string) (*Result, error) {
	pkg := overlay.Package.Name
	reserved := map[string]bool{"Provider": true}
	for name := range overlay.Resources {
		reserved[name] = true
	}
	g := &generator{
		spec: spec, overlay: overlay,
		types: newTypeBuilder(spec, pkg, reserved),
		res: &Result{Metadata: meta.Metadata{
			Resources: map[string]*meta.Resource{},
			Functions: map[string]*meta.Function{},
		}},
	}
	g.res.Schema = g.packageSpec(version)
	for _, name := range sortedKeys(overlay.Resources) {
		rs, rm, err := g.resource(name, overlay.Resources[name])
		if err != nil {
			return nil, fmt.Errorf("resource %s: %w", name, err)
		}
		tok := pkg + ":index:" + name
		g.res.Schema.Resources[tok] = rs
		g.res.Metadata.Resources[tok] = rm
	}
	for _, name := range sortedKeys(overlay.Functions) {
		fs, fm, err := g.function(name, overlay.Functions[name])
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", name, err)
		}
		tok := pkg + ":index:" + name
		g.res.Schema.Functions[tok] = fs
		g.res.Metadata.Functions[tok] = fm
	}
	g.res.Schema.Types = g.types.types
	g.res.Metadata.Types = g.types.shapes
	return g.res, nil
}

func (g *generator) warn(format string, args ...any) {
	g.res.Warnings = append(g.res.Warnings, fmt.Sprintf(format, args...))
}

func (g *generator) packageSpec(version string) pschema.PackageSpec {
	p := g.overlay.Package
	lang := func(v any) pschema.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	str := pschema.TypeSpec{Type: "string"}
	config := map[string]pschema.PropertySpec{
		"apiUrl": {
			TypeSpec:    str,
			Description: "Base URL of the Omnara API, including the version path. Defaults to https://api.omnara.com/v1. For a self-hosted deployment use `<origin>/api/v1`. Can also be set with the `OMNARA_API_URL` environment variable.",
			DefaultInfo: &pschema.DefaultSpec{Environment: []string{"OMNARA_API_URL"}},
		},
		"apiKey": {
			TypeSpec:    str,
			Secret:      true,
			Description: "An Omnara personal access token or organization API key. Can also be set with the `OMNARA_API_KEY` environment variable.",
			DefaultInfo: &pschema.DefaultSpec{Environment: []string{"OMNARA_API_KEY"}},
		},
		"orgId": {
			TypeSpec:    str,
			Description: "Default organization ID for resources and functions that don't set `orgId`. Can also be set with the `OMNARA_ORG_ID` environment variable.",
			DefaultInfo: &pschema.DefaultSpec{Environment: []string{"OMNARA_ORG_ID"}},
		},
	}
	return pschema.PackageSpec{
		Name:              p.Name,
		DisplayName:       p.DisplayName,
		Version:           version,
		Description:       p.Description,
		Keywords:          p.Keywords,
		Homepage:          p.Homepage,
		Repository:        p.Repository,
		License:           p.License,
		Publisher:         p.Publisher,
		PluginDownloadURL: p.PluginDownloadURL,
		Attribution:       fmt.Sprintf("Generated from the Omnara OpenAPI specification, version %s.", g.spec.Version),
		Meta:              &pschema.MetadataSpec{ModuleFormat: "(.*)"},
		Config:            pschema.ConfigSpec{Variables: config},
		Provider: &pschema.ResourceSpec{
			ObjectTypeSpec:  pschema.ObjectTypeSpec{Description: "The provider type for the Omnara package.", Type: "object", Properties: config},
			InputProperties: config,
		},
		Resources: map[string]pschema.ResourceSpec{},
		Functions: map[string]pschema.FunctionSpec{},
		Language: map[string]pschema.RawMessage{
			"nodejs": lang(map[string]any{
				"packageName":          "@buildbetter-app/pulumi-omnara",
				"packageDescription":   p.Description,
				"respectSchemaVersion": true,
				"dependencies":         map[string]string{"@pulumi/pulumi": "^3.142.0"},
			}),
			"python": lang(map[string]any{
				"packageName":          "pulumi_omnara",
				"respectSchemaVersion": true,
				"pyproject":            map[string]bool{"enabled": true},
				"requires":             map[string]string{"pulumi": ">=3.142.0,<4.0.0"},
			}),
			"go": lang(map[string]any{
				"importBasePath":                 "github.com/buildbetter-app/pulumi-omnara/sdk/go/omnara",
				"respectSchemaVersion":           true,
				"generateResourceContainerTypes": true,
			}),
			"csharp": lang(map[string]any{
				"rootNamespace":        "BuildBetter",
				"respectSchemaVersion": true,
				"packageReferences":    map[string]string{"Pulumi": "3.*"},
			}),
		},
	}
}

// prop is a resource or function property under construction.
type prop struct {
	spec pschema.PropertySpec
	meta *meta.Property
	// required marks required inputs.
	required bool
}

func (g *generator) paramName(param string, rename map[string]string) string {
	if n, ok := rename[param]; ok {
		return n
	}
	if n, ok := g.overlay.PathParams[param]; ok {
		return n
	}
	return camel(param)
}

func fieldName(api string, rename map[string]string) string {
	if n, ok := rename[api]; ok {
		return n
	}
	return camel(api)
}

func (g *generator) resource(name string, ro *ResourceOverlay) (pschema.ResourceSpec, *meta.Resource, error) {
	var rs pschema.ResourceSpec
	if ro.Create == nil {
		return rs, nil, fmt.Errorf("missing create")
	}
	create, err := g.spec.Op(ro.Create.Op)
	if err != nil {
		return rs, nil, err
	}
	scope := map[string]bool{}
	for _, p := range create.PathParams() {
		scope[p] = true
	}

	rm := &meta.Resource{Inputs: map[string]*meta.Property{}, Outputs: map[string]*meta.Property{}}
	inputs := map[string]*prop{}
	outputs := map[string]*prop{}

	// The resource's own ID is the path parameter that item operations add
	// to the create path.
	var itemOps []*Op
	for _, ref := range append([]*OpRef{ro.Read, ro.Delete}, ro.Update...) {
		if ref == nil {
			continue
		}
		op, err := g.spec.Op(ref.Op)
		if err != nil {
			return rs, nil, err
		}
		itemOps = append(itemOps, op)
	}
	for _, op := range itemOps {
		for _, p := range op.PathParams() {
			if scope[p] {
				continue
			}
			if rm.IDParam != "" && rm.IDParam != p {
				return rs, nil, fmt.Errorf("operations disagree on the ID parameter (%s vs %s)", rm.IDParam, p)
			}
			rm.IDParam = p
		}
	}

	// Path parameters of the create operation scope the resource.
	for _, p := range create.PathParams() {
		pn := g.paramName(p, ro.Rename)
		param, _ := g.spec.Param(create, p)
		desc := param.Description
		if desc == "" {
			desc = fmt.Sprintf("The %s this resource belongs to.", strings.TrimSuffix(pn, "Id")+" ID")
		}
		mp := &meta.Property{API: p, Shape: primPrim, Replace: true, Scope: true}
		pr := &prop{spec: pschema.PropertySpec{TypeSpec: pschema.TypeSpec{Type: "string"}, Description: desc, ReplaceOnChanges: true}, meta: mp, required: true}
		if pn == "orgId" {
			mp.DefaultFromConfig = "orgId"
			pr.required = false
			pr.spec.Description += " Defaults to the provider's `orgId`."
		}
		inputs[pn] = pr
		rm.ScopeProps = append(rm.ScopeProps, pn)
	}
	if rm.IDParam == "" {
		if len(ro.ID) == 0 {
			return rs, nil, fmt.Errorf("no ID path parameter; set id in resources.yaml")
		}
		rm.IDProps = ro.ID
	}

	createOp := g.operation(create, ro.Create, rm, ro.Rename)

	// Body fields of the create request become inputs.
	skipIn := set(ro.SkipInputs)
	body, enc := g.spec.RequestBody(create)
	createOp.BodyEncoding = enc
	if body != nil {
		fields, required := g.types.objectFields(body)
		for _, api := range sortedKeys(fields) {
			f, _ := fields[api].(node)
			pn := fieldName(api, ro.Rename)
			if hidden(f) || skipIn[pn] {
				continue
			}
			pr := g.fieldProp(name, api, f)
			pr.required = required[api] && pr.spec.Default == nil && !g.nullable(f)
			if _, clash := inputs[pn]; clash {
				return rs, nil, fmt.Errorf("input %s is both a path parameter and a body field", pn)
			}
			inputs[pn] = pr
			createOp.Body = append(createOp.Body, pn)
		}
	}
	rm.Create = createOp

	// The create response describes the outputs.
	respFields, respRequired, err := g.resultFields(create, ro.Create)
	if err != nil {
		return rs, nil, err
	}
	skipOut := set(ro.SkipOutputs)
	anyOut := set(ro.AnyOutputs)
	for _, api := range sortedKeys(respFields) {
		f, _ := respFields[api].(node)
		pn := fieldName(api, ro.Rename)
		if api == "id" || hidden(f) || skipOut[pn] {
			continue
		}
		pr := g.fieldProp(name, api, f)
		if anyOut[pn] {
			pr.spec.TypeSpec = pschema.TypeSpec{Ref: anyRef}
			pr.meta.Shape = &meta.Shape{Kind: meta.KindAny}
		}
		pr.required = respRequired[api] && !g.nullable(f)
		outputs[pn] = pr
	}
	if _, ok := respFields["id"]; !ok && rm.IDParam != "" {
		g.warn("%s: create response has no id field", name)
	}
	if len(ro.Create.Extra) > 0 {
		whole, _ := g.types.objectFields(g.spec.Response(create))
		for _, api := range sortedKeys(ro.Create.Extra) {
			pn := ro.Create.Extra[api]
			f, _ := whole[api].(node)
			if f == nil {
				return rs, nil, fmt.Errorf("create response has no field %q", api)
			}
			pr := g.fieldProp(name, api, f)
			pr.required = true
			outputs[pn] = pr
			createOp.Extra[api] = pn
		}
	}

	// Update operations decide which inputs change in place.
	updatable := map[string]bool{}
	for _, ref := range ro.Update {
		op, _ := g.spec.Op(ref.Op)
		uo := g.operation(op, ref, rm, ro.Rename)
		ubody, uenc := g.spec.RequestBody(op)
		uo.BodyEncoding = uenc
		fields, _ := g.types.objectFields(ubody)
		byAPI := map[string]string{}
		for pn, in := range inputs {
			if !in.meta.Scope {
				byAPI[in.meta.API] = pn
			}
		}
		for _, api := range sortedKeys(fields) {
			f, _ := fields[api].(node)
			if _, fromState := ref.FromState[api]; fromState || hidden(f) {
				continue
			}
			pn, ok := byAPI[api]
			if !ok {
				if !skipIn[fieldName(api, ro.Rename)] {
					g.warn("%s: update %s field %q is not a create input; skipped", name, op.ID, api)
				}
				continue
			}
			uo.Body = append(uo.Body, pn)
			updatable[pn] = true
			if g.nullable(f) {
				uo.Nullable = append(uo.Nullable, pn)
			}
		}
		rm.Updates = append(rm.Updates, uo)
	}
	if ro.Read != nil {
		op, _ := g.spec.Op(ro.Read.Op)
		rd := g.operation(op, ro.Read, rm, ro.Rename)
		rd.List = ro.Read.Match != nil
		if rd.List {
			if err := g.checkListRead(op, ro.Read); err != nil {
				return rs, nil, err
			}
		}
		rm.Read = rd
	} else {
		g.warn("%s: no read operation; refresh and import are unsupported", name)
	}
	if ro.Delete != nil {
		op, _ := g.spec.Op(ro.Delete.Op)
		rm.Delete = g.operation(op, ro.Delete, rm, ro.Rename)
	}

	secret := set(ro.Secret)
	for pn, in := range inputs {
		in.meta.Replace = in.meta.Scope || !updatable[pn]
		in.spec.ReplaceOnChanges = in.meta.Replace
		if secret[pn] {
			in.spec.Secret, in.meta.Secret = true, true
		}
	}
	for pn, out := range outputs {
		if secret[pn] {
			out.spec.Secret, out.meta.Secret = true, true
		}
		out.meta.Computed = inputs[pn] == nil
	}
	// Every input is also an output. Inputs the API never returns, such as
	// secret material, keep the value from the program.
	for pn, in := range inputs {
		if _, ok := outputs[pn]; ok {
			continue
		}
		cp := *in.meta
		outputs[pn] = &prop{spec: in.spec, meta: &cp, required: in.required || in.meta.Scope}
	}
	if ro.Unique != nil {
		rm.UniqueProps = *ro.Unique
	} else if _, ok := inputs["name"]; ok {
		rm.UniqueProps = []string{"name"}
	}
	rm.DeleteBeforeReplace = ro.DeleteBeforeReplace
	for in, out := range ro.ReadInputs {
		if inputs[in] == nil || outputs[out] == nil {
			return rs, nil, fmt.Errorf("readInputs %s: %s must be an input and %s an output", in, in, out)
		}
	}
	rm.InputFromOutput = ro.ReadInputs
	for in := range ro.ServerKeys {
		if inputs[in] == nil || inputs[in].meta.Shape.Kind != meta.KindMap {
			return rs, nil, fmt.Errorf("serverKeys %s: not a map input", in)
		}
	}
	rm.ServerKeys = ro.ServerKeys

	desc := ro.Description
	if desc == "" {
		desc = create.Description
	}
	if desc == "" {
		desc = create.Summary
	}
	rs = pschema.ResourceSpec{
		ObjectTypeSpec:  pschema.ObjectTypeSpec{Description: desc, Type: "object", Properties: map[string]pschema.PropertySpec{}},
		InputProperties: map[string]pschema.PropertySpec{},
	}
	for _, pn := range sortedKeys(inputs) {
		in := inputs[pn]
		rs.InputProperties[pn] = in.spec
		rm.Inputs[pn] = in.meta
		if in.required {
			rs.RequiredInputs = append(rs.RequiredInputs, pn)
		}
	}
	for _, pn := range sortedKeys(outputs) {
		out := outputs[pn]
		spec := out.spec
		spec.Default, spec.DefaultInfo, spec.ReplaceOnChanges = nil, nil, false
		rs.Properties[pn] = spec
		rm.Outputs[pn] = out.meta
		if out.required {
			rs.Required = append(rs.Required, pn)
		}
	}
	for _, pn := range rm.IDProps {
		if inputs[pn] == nil {
			return rs, nil, fmt.Errorf("id property %q is not an input", pn)
		}
	}
	return rs, rm, nil
}

// operation builds the metadata for one HTTP call.
func (g *generator) operation(op *Op, ref *OpRef, rm *meta.Resource, rename map[string]string) *meta.Operation {
	mo := &meta.Operation{
		OperationID: op.ID, Method: op.Method, Path: op.Path,
		PathParams: map[string]string{}, Extra: map[string]string{},
		Result: ref.Result, FromState: ref.FromState, Match: ref.Match, GoneIfSet: ref.GoneIfSet,
		Paginate: ref.Paginate, Query: ref.Query, QueryDefaults: ref.QueryDefaults,
	}
	if _, ok := g.spec.Param(op, "Idempotency-Key"); ok {
		mo.Idempotent = true
	}
	if _, ok := g.spec.Param(op, "cursor"); ok {
		mo.Paged = true
	}
	for _, p := range op.PathParams() {
		if rm != nil && p == rm.IDParam {
			mo.PathParams[p] = "$id"
		} else {
			mo.PathParams[p] = g.paramName(p, rename)
		}
	}
	return mo
}

// checkListRead confirms that list items (after result) carry every match
// field, so a wrapped item shape fails generation instead of reading as gone.
func (g *generator) checkListRead(op *Op, ref *OpRef) error {
	whole, _ := g.types.objectFields(g.spec.Response(op))
	data, _ := whole["data"].(node)
	if data == nil {
		return fmt.Errorf("%s: list response has no data field", op.ID)
	}
	item, _ := data["items"].(node)
	fields, _ := g.types.objectFields(item)
	if ref.Result != "" {
		inner, ok := fields[ref.Result]
		if !ok {
			return fmt.Errorf("%s: list items have no field %q", op.ID, ref.Result)
		}
		fields, _ = g.types.objectFields(inner)
	}
	for field := range ref.Match {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("%s: list items have no match field %q (set result if items are wrapped)", op.ID, field)
		}
	}
	for _, p := range g.spec.Params(op) {
		if p.In != "query" || !p.Required {
			continue
		}
		_, mapped := ref.Query[p.Name]
		_, defaulted := ref.QueryDefaults[p.Name]
		if !mapped || !defaulted {
			return fmt.Errorf("%s: required query parameter %q needs both query and queryDefaults (imports have no inputs)", op.ID, p.Name)
		}
	}
	return nil
}

// resultFields returns the fields of the object an operation returns.
func (g *generator) resultFields(op *Op, ref *OpRef) (map[string]any, map[string]bool, error) {
	resp := g.spec.Response(op)
	if resp == nil {
		return nil, nil, fmt.Errorf("%s has no JSON response", op.ID)
	}
	if ref.Result != "" {
		whole, _ := g.types.objectFields(resp)
		inner, ok := whole[ref.Result]
		if !ok {
			return nil, nil, fmt.Errorf("%s response has no field %q", op.ID, ref.Result)
		}
		resp = inner
	}
	fields, required := g.types.objectFields(resp)
	return fields, required, nil
}

// fieldProp maps one API field into a property.
func (g *generator) fieldProp(owner, api string, f node) *prop {
	t := g.types.typeOf(f, owner+pascal(api))
	ps := pschema.PropertySpec{TypeSpec: t.spec, Description: g.types.describeField(f)}
	shape := t.shape
	if t.binary {
		ps.TypeSpec = pschema.TypeSpec{Ref: "pulumi.json#/Archive"}
		shape = &meta.Shape{Kind: meta.KindArchive}
		if ps.Description == "" {
			ps.Description = "Archive to upload."
		}
	}
	if d, ok := f["default"]; ok && isScalar(d) && shape.Kind == meta.KindPrimitive {
		ps.Default = d
	}
	mp := &meta.Property{API: api, Shape: shape}
	if secretFieldNames[api] || f["writeOnly"] == true {
		ps.Secret, mp.Secret = true, true
	}
	return &prop{spec: ps, meta: mp}
}

func (g *generator) nullable(f node) bool {
	return g.types.nullable(f)
}

func (g *generator) function(name string, ref *OpRef) (pschema.FunctionSpec, *meta.Function, error) {
	var fs pschema.FunctionSpec
	op, err := g.spec.Op(ref.Op)
	if err != nil {
		return fs, nil, err
	}
	fm := &meta.Function{Inputs: map[string]*meta.Property{}, Outputs: map[string]*meta.Property{}}
	mo := g.operation(op, ref, nil, nil)
	mo.Query = map[string]string{}
	in := pschema.ObjectTypeSpec{Type: "object", Properties: map[string]pschema.PropertySpec{}}
	for _, p := range op.PathParams() {
		pn := g.paramName(p, nil)
		param, _ := g.spec.Param(op, p)
		ps := pschema.PropertySpec{TypeSpec: pschema.TypeSpec{Type: "string"}, Description: param.Description}
		mp := &meta.Property{API: p, Shape: primPrim, Scope: true}
		if pn == "orgId" {
			mp.DefaultFromConfig = "orgId"
			ps.Description = strings.TrimSpace(ps.Description + " Defaults to the provider's `orgId`.")
		} else {
			in.Required = append(in.Required, pn)
		}
		in.Properties[pn] = ps
		fm.Inputs[pn] = mp
	}
	for _, p := range g.spec.Params(op) {
		if p.In != "query" || (ref.Paginate && (p.Name == "limit" || p.Name == "cursor")) {
			continue
		}
		pn := camel(p.Name)
		t := g.types.typeOf(p.Schema, pascal(name)+pascal(p.Name))
		if t.shape.Kind != meta.KindPrimitive && !(t.shape.Kind == meta.KindArray && t.shape.Items.Kind == meta.KindPrimitive) {
			g.warn("%s: query parameter %s has a complex type; skipped", name, p.Name)
			continue
		}
		in.Properties[pn] = pschema.PropertySpec{TypeSpec: t.spec, Description: p.Description}
		fm.Inputs[pn] = &meta.Property{API: p.Name, Shape: t.shape}
		mo.Query[p.Name] = pn
		if p.Required {
			in.Required = append(in.Required, pn)
		}
	}
	fm.Operation = mo

	out := pschema.ObjectTypeSpec{Type: "object", Properties: map[string]pschema.PropertySpec{}}
	fields, required, err := g.resultFields(op, ref)
	if err != nil {
		return fs, nil, err
	}
	if ref.Paginate {
		data, _ := fields["data"].(node)
		if data == nil {
			return fs, nil, fmt.Errorf("%s has no data field to paginate", op.ID)
		}
		pr := g.fieldProp(pascal(name), "items", data)
		pr.spec.Description = "Every item across all pages."
		out.Properties["items"] = pr.spec
		out.Required = []string{"items"}
		pr.meta.API = "data"
		fm.Outputs["items"] = pr.meta
		fm.ItemsProp = "items"
	} else {
		for _, api := range sortedKeys(fields) {
			f, _ := fields[api].(node)
			if hidden(f) {
				continue
			}
			pn := camel(api)
			pr := g.fieldProp(pascal(name), api, f)
			out.Properties[pn] = pr.spec
			fm.Outputs[pn] = pr.meta
			if required[api] && !g.nullable(f) {
				out.Required = append(out.Required, pn)
			}
		}
	}
	sort.Strings(in.Required)
	sort.Strings(out.Required)
	fs = pschema.FunctionSpec{Description: firstNonEmpty(op.Description, op.Summary), Inputs: &in, Outputs: &out}
	return fs, fm, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}
