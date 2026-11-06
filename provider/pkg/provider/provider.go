// Package provider implements the Omnara Pulumi provider. Resources and
// functions are driven by the metadata that pulumi-gen-omnara writes from
// the Omnara OpenAPI document, so most API changes only need regeneration.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"slices"
	"strings"

	p "github.com/pulumi/pulumi-go-provider"
	presource "github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"

	"github.com/buildbetter-app/pulumi-omnara/provider/pkg/meta"
)

// Name is the Pulumi package name.
const Name = "omnara"

type omnara struct {
	version string
	schema  string
	md      *meta.Metadata
	conv    *converter
	// outputByAPI maps, per resource token, API fields to output names.
	outputByAPI map[string]map[string]string

	client *client
	orgID  string
	// orgUnknown is set during previews when orgId config is not yet known.
	orgUnknown bool
}

// New builds the provider from the generated schema and metadata.
func New(version string, schemaJSON, metadataJSON []byte) (p.Provider, error) {
	var md meta.Metadata
	if err := json.Unmarshal(metadataJSON, &md); err != nil {
		return p.Provider{}, fmt.Errorf("decode metadata: %w", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return p.Provider{}, fmt.Errorf("decode schema: %w", err)
	}
	schema["version"] = version
	stamped, err := json.Marshal(schema)
	if err != nil {
		return p.Provider{}, err
	}
	var shapes []*meta.Shape
	for _, res := range md.Resources {
		for _, pm := range res.Inputs {
			shapes = append(shapes, pm.Shape)
		}
		for _, pm := range res.Outputs {
			shapes = append(shapes, pm.Shape)
		}
	}
	for _, fn := range md.Functions {
		for _, pm := range fn.Outputs {
			shapes = append(shapes, pm.Shape)
		}
	}
	o := &omnara{
		version: version, schema: string(stamped), md: &md,
		conv:        newConverter(md.Types, shapes...),
		outputByAPI: map[string]map[string]string{},
	}
	for tok, res := range md.Resources {
		m := map[string]string{}
		for pn, pm := range res.Outputs {
			if !pm.Scope {
				m[pm.API] = pn
			}
		}
		o.outputByAPI[tok] = m
	}
	return p.Provider{
		GetSchema: func(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
			return p.GetSchemaResponse{Schema: o.schema}, nil
		},
		CheckConfig: func(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
			return p.CheckResponse{Inputs: req.Inputs}, nil
		},
		DiffConfig: o.diffConfig,
		Configure:  o.configure,
		Check:      o.check,
		Diff:       o.diff,
		Create:     o.create,
		Read:       o.read,
		Update:     o.update,
		Delete:     o.delete,
		Invoke:     o.invoke,
	}, nil
}

func (o *omnara) configure(_ context.Context, req p.ConfigureRequest) error {
	get := func(key, env string) (string, bool) {
		if v, ok := req.Args.GetOk(key); ok {
			if v.IsComputed() {
				return "", false
			}
			if v.IsString() && v.AsString() != "" {
				return v.AsString(), true
			}
		}
		return os.Getenv(env), true
	}
	apiURL, _ := get("apiUrl", "OMNARA_API_URL")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	apiKey, _ := get("apiKey", "OMNARA_API_KEY")
	orgID, known := get("orgId", "OMNARA_ORG_ID")
	o.orgID, o.orgUnknown = orgID, !known
	o.client = newClient(apiURL, apiKey, "pulumi-omnara/"+o.version)
	return nil
}

func (o *omnara) diffConfig(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
	detailed := map[string]p.PropertyDiff{}
	for _, k := range []string{"apiUrl", "apiKey", "orgId"} {
		if !sameValue(req.OldInputs.Get(k), req.Inputs.Get(k)) {
			detailed[k] = p.PropertyDiff{Kind: p.Update, InputDiff: true}
		}
	}
	return p.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (o *omnara) resource(urn presource.URN) (*meta.Resource, string, error) {
	tok := string(urn.Type())
	res, ok := o.md.Resources[tok]
	if !ok {
		return nil, tok, fmt.Errorf("unknown resource type %q", tok)
	}
	return res, tok, nil
}

func (o *omnara) check(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
	res, _, err := o.resource(req.Urn)
	if err != nil {
		return p.CheckResponse{}, err
	}
	inputs, failures := o.applyConfigDefaults(res.Inputs, req.Inputs)
	// The engine sends archive contents to Check but not to Diff, so read
	// the skill name here; Diff compares it with the live name.
	for _, pn := range sortedKeys(res.Inputs) {
		v := inputs.Get(pn)
		if res.Inputs[pn].Shape.Kind != meta.KindArchive || !v.IsArchive() || v.HasComputed() {
			continue
		}
		name, err := skillArchiveName(v.AsArchive())
		if err != nil {
			failures = append(failures, p.CheckFailure{Property: pn, Reason: err.Error()})
			continue
		}
		inputs = inputs.Set(skillNameInput, property.New(name))
	}
	return p.CheckResponse{Inputs: inputs, Failures: failures}, nil
}

// skillNameInput is a hidden input that carries the SKILL.md name from
// Check to Diff.
const skillNameInput = "__skillName"

// applyConfigDefaults fills inputs such as orgId from provider config.
func (o *omnara) applyConfigDefaults(props map[string]*meta.Property, inputs property.Map) (property.Map, []p.CheckFailure) {
	var failures []p.CheckFailure
	for _, pn := range sortedKeys(props) {
		if props[pn].DefaultFromConfig == "" {
			continue
		}
		if v, ok := inputs.GetOk(pn); ok && !v.IsNull() {
			continue
		}
		switch {
		case o.orgID != "":
			inputs = inputs.Set(pn, property.New(o.orgID))
		case o.orgUnknown:
			inputs = inputs.Set(pn, property.New(property.Computed))
		default:
			failures = append(failures, p.CheckFailure{Property: pn,
				Reason: "orgId is required: set it on the resource, or set the provider's orgId (omnara:orgId or OMNARA_ORG_ID)"})
		}
	}
	return inputs, failures
}

func (o *omnara) diff(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
	res, _, err := o.resource(req.Urn)
	if err != nil {
		return p.DiffResponse{}, err
	}
	olds, news := req.OldInputs, req.Inputs
	detailed := map[string]p.PropertyDiff{}
	replace := false
	for _, pn := range sortedKeys(res.Inputs) {
		pm := res.Inputs[pn]
		ov, nv := olds.Get(pn), news.Get(pn)
		oOK, nOK := !isEmpty(ov), !isEmpty(nv)
		if !oOK && !nOK {
			continue
		}
		if oOK && nOK && !nv.HasComputed() && sameValue(ov, nv) {
			continue
		}
		kind := p.Update
		switch {
		case !oOK:
			kind = p.Add
		case !nOK:
			kind = p.Delete
		}
		if kind == p.Delete && !pm.Replace && clearMode(res, pn) == "" {
			// The API can't clear this field, so dropping it from the
			// program leaves the live value as it is.
			continue
		}
		forceReplace := pm.Replace
		if pm.Shape.Kind == meta.KindArchive {
			// A skill's name comes from SKILL.md and can't change in place.
			name, cur := news.Get(skillNameInput), req.State.Get("name")
			forceReplace = forceReplace || (name.IsString() && cur.IsString() && name.AsString() != cur.AsString())
		}
		if forceReplace {
			replace = true
			kind = map[p.DiffKind]p.DiffKind{p.Add: p.AddReplace, p.Delete: p.DeleteReplace, p.Update: p.UpdateReplace}[kind]
		}
		detailed[pn] = p.PropertyDiff{Kind: kind, InputDiff: true}
	}
	resp := p.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}
	switch {
	case replace && res.DeleteBeforeReplace:
		resp.DeleteBeforeReplace = true
	case replace && len(res.UniqueProps) > 0:
		// A replacement that keeps a unique value (such as the name) would
		// collide with the old resource, so delete the old one first.
		resp.DeleteBeforeReplace = true
		for _, u := range res.UniqueProps {
			if _, changed := detailed[u]; changed {
				resp.DeleteBeforeReplace = false
			}
		}
	}
	return resp, nil
}

// clearMode says how an update clears an input the program removed: send
// null, an empty map, or an empty list. It is empty when no update operation
// can clear the field.
func clearMode(res *meta.Resource, pn string) string {
	pm := res.Inputs[pn]
	for _, op := range res.Updates {
		if !slices.Contains(op.Body, pn) {
			continue
		}
		switch {
		case slices.Contains(op.Nullable, pn):
			return "null"
		case pm.Shape.Kind == meta.KindMap:
			return "map"
		case pm.Shape.Kind == meta.KindArray:
			return "array"
		}
	}
	return ""
}

// isEmpty treats null, an empty map, and an empty list like an unset value.
func isEmpty(v property.Value) bool {
	switch {
	case v.IsNull():
		return true
	case v.IsMap():
		return v.AsMap().Len() == 0
	case v.IsArray():
		return v.AsArray().Len() == 0
	}
	return false
}

func changed(a, b property.Value) bool {
	if isEmpty(a) && isEmpty(b) {
		return false
	}
	return b.HasComputed() || !sameValue(a, b)
}

func (o *omnara) create(ctx context.Context, req p.CreateRequest) (p.CreateResponse, error) {
	res, tok, err := o.resource(req.Urn)
	if err != nil {
		return p.CreateResponse{}, err
	}
	inputs := req.Properties
	if req.DryRun {
		return p.CreateResponse{Properties: o.previewOutputs(res, inputs)}, nil
	}
	op := res.Create
	vals, err := scopeValues(res, inputs)
	if err != nil {
		return p.CreateResponse{}, err
	}
	path, err := buildPath(op, vals)
	if err != nil {
		return p.CreateResponse{}, err
	}
	body, ctype, err := o.encodeBody(op, res, op.Body, inputs, false, nil)
	if err != nil {
		return p.CreateResponse{}, err
	}
	raw, err := o.client.do(ctx, request{method: op.Method, path: path, body: body, contentType: ctype, idempotent: op.Idempotent})
	if err != nil {
		return p.CreateResponse{}, err
	}
	whole, obj, err := decodeResult(raw, op.Result)
	if err != nil {
		return p.CreateResponse{}, err
	}
	id := ""
	if res.IDParam != "" {
		id, _ = obj["id"].(string)
		if id == "" {
			return p.CreateResponse{}, fmt.Errorf("%s: create response has no id", tok)
		}
	} else {
		id = compositeID(res, vals)
	}
	return p.CreateResponse{ID: id, Properties: o.outputs(tok, res, property.Map{}, inputs, obj, whole, op.Extra)}, nil
}

func (o *omnara) read(ctx context.Context, req p.ReadRequest) (p.ReadResponse, error) {
	res, tok, err := o.resource(req.Urn)
	if err != nil {
		return p.ReadResponse{}, err
	}
	if res.Read == nil {
		return p.ReadResponse{ID: req.ID, Properties: req.Properties, Inputs: req.Inputs}, nil
	}
	vals, err := o.locate(res, req.ID, req.Inputs, req.Properties)
	if err != nil {
		return p.ReadResponse{}, err
	}
	obj, err := o.fetch(ctx, res.Read, vals, req.Inputs, req.Properties)
	if err != nil || obj == nil {
		return p.ReadResponse{}, err
	}
	base := req.Properties
	for _, pn := range res.ScopeProps {
		base = base.Set(pn, property.New(vals[pn]))
	}
	outputs := o.outputs(tok, res, base, property.Map{}, obj, obj, nil)
	id := vals["$id"]
	if res.IDParam == "" {
		id = compositeID(res, vals)
	}
	return p.ReadResponse{ID: id, Properties: outputs, Inputs: o.readInputs(res, req.Inputs, outputs, obj, vals)}, nil
}

// readInputs refreshes inputs from the live object. During refresh it only
// touches what the program set: inputs, map entries, and object fields the
// previous inputs had. On import it fills every input the API returns.
func (o *omnara) readInputs(res *meta.Resource, old, outputs property.Map, obj map[string]any, vals map[string]string) property.Map {
	importing := old.Len() == 0
	in := old
	for _, pn := range sortedKeys(res.Inputs) {
		pm := res.Inputs[pn]
		if pm.Scope {
			if pm.DefaultFromConfig != "" && !importing {
				continue
			}
			in = in.Set(pn, property.New(vals[pn]))
			continue
		}
		prev, had := old.GetOk(pn)
		had = had && !prev.IsNull()
		if (!importing && !had) || pm.Secret {
			continue
		}
		src := pn
		if from, ok := res.InputFromOutput[pn]; ok {
			src = from
		}
		out := res.Outputs[src]
		if out == nil || out.Scope {
			continue
		}
		raw, returned := obj[out.API]
		if !returned {
			continue
		}
		if raw == nil {
			// Cleared outside Pulumi. Dropping the input lets the next
			// update set it again.
			if !importing {
				in = in.Delete(pn)
			}
			continue
		}
		v := outputs.Get(src)
		if !sameShape(out.Shape, pm.Shape) {
			// The API reports some objects in a richer shape than it accepts
			// (an owner with names and IDs, for example). Keep the fields the
			// input type declares.
			var ok bool
			if v, ok = o.conv.project(v, pm.Shape); !ok {
				continue
			}
		}
		v = withoutKeys(v, res.ServerKeys[pn])
		if importing {
			if isEmpty(v) {
				continue
			}
		} else {
			v = o.conv.restrict(prev, v, pm.Shape)
		}
		in = in.Set(pn, copySecrets(v, prev))
	}
	return in
}

func (o *omnara) update(ctx context.Context, req p.UpdateRequest) (p.UpdateResponse, error) {
	res, tok, err := o.resource(req.Urn)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	olds, news := req.OldInputs, req.Inputs
	out := req.State
	if req.DryRun {
		// The API sets computed outputs (revisions, timestamps, current
		// config), so they are unknown until the update runs.
		preview := o.outputs(tok, res, out, news, nil, nil, nil)
		for pn, pm := range res.Outputs {
			if pm.Computed {
				preview = preview.Set(pn, property.New(property.Computed).WithSecret(pm.Secret))
			}
		}
		return p.UpdateResponse{Properties: preview}, nil
	}
	vals, err := scopeValues(res, news)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	vals["$id"] = req.ID
	for _, op := range res.Updates {
		// Omnara's PATCH and PUT updates are both partial: send only what
		// changed, and clear removed fields where the API allows it.
		var send []string
		for _, pn := range op.Body {
			if changed(olds.Get(pn), news.Get(pn)) && (!isEmpty(news.Get(pn)) || clearMode(res, pn) != "") {
				send = append(send, pn)
			}
		}
		if len(send) == 0 {
			continue
		}
		extra := map[string]any{}
		for api, pn := range op.FromState {
			j, err := o.conv.toJSON(out.Get(pn), nil)
			if err != nil {
				return p.UpdateResponse{}, fmt.Errorf("%s: %w", pn, err)
			}
			extra[api] = j
		}
		path, err := buildPath(op, vals)
		if err != nil {
			return p.UpdateResponse{}, err
		}
		body, ctype, err := o.encodeBody(op, res, send, news, true, extra)
		if err != nil {
			return p.UpdateResponse{}, err
		}
		raw, err := o.client.do(ctx, request{method: op.Method, path: path, body: body, contentType: ctype, idempotent: op.Idempotent})
		if err != nil {
			return p.UpdateResponse{Properties: out}, err
		}
		var whole, obj map[string]any
		if len(bytes.TrimSpace(raw)) > 0 {
			if whole, obj, err = decodeResult(raw, op.Result); err != nil {
				return p.UpdateResponse{Properties: out}, err
			}
		}
		out = o.outputs(tok, res, out, news, obj, whole, nil)
	}
	// Inputs no update call carried still land in state.
	return p.UpdateResponse{Properties: o.outputs(tok, res, out, news, nil, nil, nil)}, nil
}

func (o *omnara) delete(ctx context.Context, req p.DeleteRequest) error {
	res, _, err := o.resource(req.Urn)
	if err != nil {
		return err
	}
	if res.Delete == nil {
		return nil
	}
	vals, err := o.locate(res, req.ID, req.OldInputs, req.Properties)
	if err != nil {
		return err
	}
	path, err := buildPath(res.Delete, vals)
	if err != nil {
		return err
	}
	_, err = o.client.do(ctx, request{method: res.Delete.Method, path: path})
	if isStatus(err, http.StatusNotFound, http.StatusGone) {
		return nil
	}
	return err
}

func (o *omnara) invoke(ctx context.Context, req p.InvokeRequest) (p.InvokeResponse, error) {
	fn, ok := o.md.Functions[string(req.Token)]
	if !ok {
		return p.InvokeResponse{}, fmt.Errorf("unknown function %q", req.Token)
	}
	args, failures := o.applyConfigDefaults(fn.Inputs, req.Args)
	if len(failures) > 0 {
		return p.InvokeResponse{Failures: failures}, nil
	}
	op := fn.Operation
	vals := map[string]string{}
	query := url.Values{}
	for pn, pm := range fn.Inputs {
		v, ok := args.GetOk(pn)
		if !ok || v.IsNull() {
			continue
		}
		if pm.Scope {
			if !v.IsString() {
				return p.InvokeResponse{}, fmt.Errorf("%s must be a string", pn)
			}
			vals[pn] = v.AsString()
			continue
		}
		items := []property.Value{v}
		if v.IsArray() {
			items = v.AsArray().AsSlice()
		}
		for _, e := range items {
			j, err := o.conv.toJSON(e, nil)
			if err != nil {
				return p.InvokeResponse{}, fmt.Errorf("%s: %w", pn, err)
			}
			query.Add(pm.API, fmt.Sprint(j))
		}
	}
	path, err := buildPath(op, vals)
	if err != nil {
		return p.InvokeResponse{}, err
	}
	out := map[string]property.Value{}
	if op.Paginate {
		items, err := o.listAll(ctx, op, path, query, nil)
		if err != nil {
			return p.InvokeResponse{}, err
		}
		all := make([]any, len(items))
		for i, it := range items {
			all[i] = it
		}
		out[fn.ItemsProp] = o.conv.fromJSON(all, fn.Outputs[fn.ItemsProp].Shape)
		return p.InvokeResponse{Return: property.NewMap(out)}, nil
	}
	raw, err := o.client.do(ctx, request{method: op.Method, path: path, query: query})
	if err != nil {
		return p.InvokeResponse{}, err
	}
	_, obj, err := decodeResult(raw, op.Result)
	if err != nil {
		return p.InvokeResponse{}, err
	}
	for pn, pm := range fn.Outputs {
		if j, ok := obj[pm.API]; ok {
			out[pn] = o.conv.fromJSON(j, pm.Shape)
		}
	}
	return p.InvokeResponse{Return: property.NewMap(out)}, nil
}

// locate resolves the path values for an existing resource. During import
// the ID carries the scope values: "<orgId>/<projectId>/<id>", where orgId
// may be omitted when the provider sets it.
func (o *omnara) locate(res *meta.Resource, id string, sources ...property.Map) (map[string]string, error) {
	vals := map[string]string{"$id": id}
	missing := false
	for _, pn := range res.ScopeProps {
		found := false
		for _, src := range sources {
			if v, ok := src.GetOk(pn); ok && v.IsString() && v.AsString() != "" {
				vals[pn], found = v.AsString(), true
				break
			}
		}
		missing = missing || !found
	}
	if !missing {
		return vals, nil
	}
	parts := strings.Split(id, "/")
	want := len(res.ScopeProps)
	if res.IDParam != "" {
		want++
	}
	if len(parts) == want-1 && len(res.ScopeProps) > 0 && res.ScopeProps[0] == "orgId" && o.orgID != "" {
		parts = append([]string{o.orgID}, parts...)
	}
	if len(parts) != want {
		format := strings.Join(res.ScopeProps, "/")
		if res.IDParam != "" {
			format += "/<id>"
		}
		return nil, fmt.Errorf("cannot locate %q: import IDs have the form %s", id, format)
	}
	for i, pn := range res.ScopeProps {
		vals[pn] = parts[i]
	}
	if res.IDParam != "" {
		vals["$id"] = parts[len(parts)-1]
	}
	return vals, nil
}

// fetch reads the live object, or returns nil when it no longer exists.
// Query parameters come from the first source that has a string value.
func (o *omnara) fetch(ctx context.Context, op *meta.Operation, vals map[string]string, sources ...property.Map) (map[string]any, error) {
	path, err := buildPath(op, vals)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for param, ref := range op.Query {
		if v, ok := lookup(ref, sources...); ok {
			query.Set(param, v)
		}
	}
	for param, v := range op.QueryDefaults {
		if query.Get(param) == "" {
			query.Set(param, v)
		}
	}
	var obj map[string]any
	if op.List {
		items, err := o.listAll(ctx, op, path, query, func(item map[string]any) bool {
			return matches(unwrap(item, op.Result), op.Match, vals)
		})
		if isStatus(err, http.StatusNotFound, http.StatusGone) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			obj = unwrap(items[0], op.Result)
		}
	} else {
		raw, err := o.client.do(ctx, request{method: op.Method, path: path, query: query})
		if isStatus(err, http.StatusNotFound, http.StatusGone) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if _, obj, err = decodeResult(raw, op.Result); err != nil {
			return nil, err
		}
	}
	if obj != nil && op.GoneIfSet != "" && obj[op.GoneIfSet] != nil {
		return nil, nil
	}
	return obj, nil
}

// listAll pages through a list. When stop is set it returns the first
// item stop accepts.
func (o *omnara) listAll(ctx context.Context, op *meta.Operation, path string, query url.Values, stop func(map[string]any) bool) ([]map[string]any, error) {
	var items []map[string]any
	cursor := ""
	for page := 0; page < 10000; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		if op.Paged {
			q.Set("limit", "100")
			if cursor != "" {
				q.Set("cursor", cursor)
			}
		}
		raw, err := o.client.do(ctx, request{method: op.Method, path: path, query: q})
		if err != nil {
			return nil, err
		}
		var resp struct {
			Data       []map[string]any `json:"data"`
			NextCursor *string          `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		for _, it := range resp.Data {
			if stop == nil {
				items = append(items, it)
			} else if stop(it) {
				return []map[string]any{it}, nil
			}
		}
		if !op.Paged || resp.NextCursor == nil || *resp.NextCursor == "" || *resp.NextCursor == cursor {
			break
		}
		cursor = *resp.NextCursor
	}
	return items, nil
}

// unwrap returns the named object field of a list item, or the item itself.
func unwrap(item map[string]any, field string) map[string]any {
	if field == "" {
		return item
	}
	inner, _ := item[field].(map[string]any)
	return inner
}

// lookup resolves a dotted property path to a string value.
func lookup(ref string, sources ...property.Map) (string, bool) {
	for _, src := range sources {
		v := property.New(src)
		for _, part := range strings.Split(ref, ".") {
			if !v.IsMap() {
				v = property.New(property.Null)
				break
			}
			v = v.AsMap().Get(part)
		}
		if v.IsString() && v.AsString() != "" {
			return v.AsString(), true
		}
	}
	return "", false
}

func matches(item map[string]any, match map[string]string, vals map[string]string) bool {
	for field, pn := range match {
		got, _ := item[field].(string)
		if got == "" || got != vals[pn] {
			return false
		}
	}
	return true
}

// outputs layers state: base, then the inputs, then API response fields.
func (o *omnara) outputs(tok string, res *meta.Resource, base, inputs property.Map, obj, whole map[string]any, extra map[string]string) property.Map {
	out := base.AsMap()
	if out == nil {
		out = map[string]property.Value{}
	}
	for pn, v := range inputs.All {
		// An input whose output has another shape (an owner input versus
		// the owner the API reports) is left for the response to fill.
		if po, pi := res.Outputs[pn], res.Inputs[pn]; po != nil && pi != nil && (po.Scope || sameShape(po.Shape, pi.Shape)) {
			out[pn] = v
		}
	}
	byAPI := o.outputByAPI[tok]
	for api, j := range obj {
		if pn, ok := byAPI[api]; ok {
			out[pn] = o.conv.fromJSON(j, res.Outputs[pn].Shape)
		}
	}
	for api, pn := range extra {
		if j, ok := whole[api]; ok {
			out[pn] = o.conv.fromJSON(j, res.Outputs[pn].Shape)
		}
	}
	// Secrets in the inputs or the previous state stay secret, at any depth,
	// even when the API echoes the value back in plain text.
	for pn, pm := range res.Outputs {
		v, ok := out[pn]
		if !ok {
			continue
		}
		if pm.Secret {
			out[pn] = v.WithSecret(true)
			continue
		}
		if in, ok := inputs.GetOk(pn); ok {
			v = copySecrets(v, in)
		}
		if prev, ok := base.GetOk(pn); ok {
			v = copySecrets(v, prev)
		}
		out[pn] = v
	}
	delete(out, "id")
	delete(out, "urn")
	return property.NewMap(out)
}

// previewOutputs predicts outputs for a create during preview.
func (o *omnara) previewOutputs(res *meta.Resource, inputs property.Map) property.Map {
	out := map[string]property.Value{}
	for pn, pm := range res.Outputs {
		pi := res.Inputs[pn]
		if v, ok := inputs.GetOk(pn); ok && pi != nil && (pm.Scope || sameShape(pm.Shape, pi.Shape)) {
			out[pn] = v
		} else {
			out[pn] = property.New(property.Computed)
		}
		if pm.Secret {
			out[pn] = out[pn].WithSecret(true)
		}
	}
	return property.NewMap(out)
}

// encodeBody builds a request body from the named input properties. With
// clear set, unset properties are sent in the form that clears them.
func (o *omnara) encodeBody(op *meta.Operation, res *meta.Resource, props []string, values property.Map, clear bool, extra map[string]any) ([]byte, string, error) {
	if op.BodyEncoding == "" {
		return nil, "", nil
	}
	if op.BodyEncoding == "multipart" {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, pn := range props {
			pm := res.Inputs[pn]
			v, ok := values.GetOk(pn)
			if !ok || v.IsNull() {
				continue
			}
			h := textproto.MIMEHeader{}
			var content []byte
			if pm.Shape.Kind == meta.KindArchive {
				if !v.IsArchive() {
					return nil, "", fmt.Errorf("%s must be an archive", pn)
				}
				zipped, err := zipSkill(v.AsArchive())
				if err != nil {
					return nil, "", fmt.Errorf("%s: %w", pn, err)
				}
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename="skill.zip"`, pm.API))
				h.Set("Content-Type", "application/zip")
				content = zipped
			} else {
				j, err := o.conv.toJSON(v, pm.Shape)
				if err != nil {
					return nil, "", fmt.Errorf("%s: %w", pn, err)
				}
				if content, err = json.Marshal(j); err != nil {
					return nil, "", err
				}
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q`, pm.API))
				h.Set("Content-Type", "application/json")
			}
			w, err := mw.CreatePart(h)
			if err != nil {
				return nil, "", err
			}
			if _, err := w.Write(content); err != nil {
				return nil, "", err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), mw.FormDataContentType(), nil
	}
	body := map[string]any{}
	for _, pn := range props {
		pm := res.Inputs[pn]
		v, ok := values.GetOk(pn)
		if !ok || v.IsNull() {
			if clear {
				switch clearMode(res, pn) {
				case "null":
					body[pm.API] = nil
				case "map":
					body[pm.API] = map[string]any{}
				case "array":
					body[pm.API] = []any{}
				}
			}
			continue
		}
		j, err := o.conv.toJSON(v, pm.Shape)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", pn, err)
		}
		body[pm.API] = j
	}
	for k, v := range extra {
		body[k] = v
	}
	b, err := json.Marshal(body)
	return b, "application/json", err
}

func decodeResult(raw []byte, result string) (map[string]any, map[string]any, error) {
	var whole map[string]any
	if err := json.Unmarshal(raw, &whole); err != nil {
		return nil, nil, fmt.Errorf("decode response: %w", err)
	}
	if result == "" {
		return whole, whole, nil
	}
	obj, ok := whole[result].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("response has no %q object", result)
	}
	return whole, obj, nil
}

// scopeValues reads the string inputs that fill path parameters.
func scopeValues(res *meta.Resource, inputs property.Map) (map[string]string, error) {
	vals := map[string]string{}
	for _, pn := range res.ScopeProps {
		v, ok := inputs.GetOk(pn)
		if !ok || !v.IsString() || v.AsString() == "" {
			return nil, fmt.Errorf("%s is required", pn)
		}
		vals[pn] = v.AsString()
	}
	return vals, nil
}

func compositeID(res *meta.Resource, vals map[string]string) string {
	parts := make([]string, len(res.IDProps))
	for i, pn := range res.IDProps {
		parts[i] = vals[pn]
	}
	return strings.Join(parts, "/")
}

func buildPath(op *meta.Operation, vals map[string]string) (string, error) {
	path := op.Path
	for param, pn := range op.PathParams {
		v := vals[pn]
		if v == "" {
			return "", fmt.Errorf("%s: missing value for path parameter %s", op.OperationID, param)
		}
		path = strings.ReplaceAll(path, "{"+param+"}", url.PathEscape(v))
	}
	return path, nil
}

func sameShape(a, b *meta.Shape) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind || a.Ref != b.Ref || len(a.Variants) != len(b.Variants) {
		return false
	}
	for i := range a.Variants {
		if a.Variants[i] != b.Variants[i] {
			return false
		}
	}
	return sameShape(a.Items, b.Items)
}

// withoutKeys drops map entries the API adds on its own.
func withoutKeys(v property.Value, keys []string) property.Value {
	if len(keys) == 0 || !v.IsMap() {
		return v
	}
	return property.New(v.AsMap().Delete(keys...)).WithSecret(v.Secret())
}

// copySecrets marks v secret wherever ref, a value it derives from, holds a
// secret. When the two have different structure, all of v becomes secret.
func copySecrets(v, ref property.Value) property.Value {
	if !ref.HasSecrets() {
		return v
	}
	if ref.Secret() {
		return v.WithSecret(true)
	}
	switch {
	case v.IsMap() && ref.IsMap():
		rm := ref.AsMap()
		out := map[string]property.Value{}
		for k, e := range v.AsMap().All {
			if r, ok := rm.GetOk(k); ok {
				e = copySecrets(e, r)
			}
			out[k] = e
		}
		return property.New(out).WithSecret(v.Secret())
	case v.IsArray() && ref.IsArray() && v.AsArray().Len() == ref.AsArray().Len():
		items := v.AsArray().AsSlice()
		out := make([]property.Value, len(items))
		for i, e := range items {
			out[i] = copySecrets(e, ref.AsArray().Get(i))
		}
		return property.New(out).WithSecret(v.Secret())
	}
	return v.WithSecret(true)
}
