package provider

import (
	"fmt"
	"sort"

	"github.com/pulumi/pulumi/sdk/v3/go/property"

	"github.com/buildbetter-app/pulumi-omnara/provider/pkg/meta"
)

// converter maps values between Pulumi properties (camelCase) and API JSON
// (snake_case), guided by the generated shapes. Map keys are user data and
// are never renamed. It is read-only after newConverter, because the
// provider serves RPCs concurrently.
type converter struct {
	types  map[string]*meta.Object
	unions map[string]*meta.Object
	// reverse maps a type token (or union key) to Pulumi name -> API field.
	reverse map[string]map[string]string
}

// newConverter indexes the object types and merges every union reachable
// from them or from shapes.
func newConverter(types map[string]*meta.Object, shapes ...*meta.Shape) *converter {
	c := &converter{types: types, unions: map[string]*meta.Object{}, reverse: map[string]map[string]string{}}
	for tok, obj := range types {
		c.reverse[tok] = reverseFields(obj)
	}
	for _, obj := range types {
		for _, f := range obj.Props {
			c.prepare(f.Shape)
		}
	}
	for _, sh := range shapes {
		c.prepare(sh)
	}
	return c
}

func (c *converter) prepare(sh *meta.Shape) {
	for ; sh != nil; sh = sh.Items {
		if sh.Kind != meta.KindUnion || len(sh.Variants) == 0 {
			continue
		}
		key := fmt.Sprint(sh.Variants)
		if _, ok := c.unions[key]; ok {
			continue
		}
		// A union value is converted with the fields of every variant; the
		// variants of one union never disagree on a field's Pulumi name.
		merged := &meta.Object{Props: map[string]*meta.Field{}}
		for _, tok := range sh.Variants {
			if obj := c.types[tok]; obj != nil {
				for api, f := range obj.Props {
					if _, ok := merged.Props[api]; !ok {
						merged.Props[api] = f
					}
				}
			}
		}
		c.unions[key] = merged
		c.reverse[key] = reverseFields(merged)
	}
}

func reverseFields(obj *meta.Object) map[string]string {
	r := make(map[string]string, len(obj.Props))
	for api, f := range obj.Props {
		r[f.Name] = api
	}
	return r
}

// object returns the field mapping for a ref or union shape.
func (c *converter) object(sh *meta.Shape) (*meta.Object, map[string]string) {
	switch sh.Kind {
	case meta.KindRef:
		return c.types[sh.Ref], c.reverse[sh.Ref]
	case meta.KindUnion:
		key := fmt.Sprint(sh.Variants)
		return c.unions[key], c.reverse[key]
	}
	return nil, nil
}

// toJSON converts a Pulumi value into a JSON-ready Go value.
func (c *converter) toJSON(v property.Value, sh *meta.Shape) (any, error) {
	switch {
	case v.IsNull():
		return nil, nil
	case v.IsComputed():
		return nil, fmt.Errorf("value is unknown")
	case v.IsString():
		return v.AsString(), nil
	case v.IsNumber():
		return v.AsNumber(), nil
	case v.IsBool():
		return v.AsBool(), nil
	case v.IsArray():
		var items *meta.Shape
		if sh != nil && sh.Kind == meta.KindArray {
			items = sh.Items
		}
		out := make([]any, 0, v.AsArray().Len())
		for _, e := range v.AsArray().All {
			j, err := c.toJSON(e, items)
			if err != nil {
				return nil, err
			}
			out = append(out, j)
		}
		return out, nil
	case v.IsMap():
		var obj *meta.Object
		var rev map[string]string
		var values *meta.Shape
		if sh != nil {
			switch sh.Kind {
			case meta.KindRef, meta.KindUnion:
				obj, rev = c.object(sh)
			case meta.KindMap:
				values = sh.Items
			}
		}
		out := map[string]any{}
		for k, e := range v.AsMap().All {
			key, fieldShape := k, values
			if obj != nil {
				if api, ok := rev[k]; ok {
					key, fieldShape = api, obj.Props[api].Shape
				}
			}
			j, err := c.toJSON(e, fieldShape)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[key] = j
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported value %v", v)
}

// fromJSON converts a decoded JSON value into a Pulumi value.
func (c *converter) fromJSON(j any, sh *meta.Shape) property.Value {
	switch x := j.(type) {
	case nil:
		return property.New(property.Null)
	case string:
		return property.New(x)
	case float64:
		return property.New(x)
	case bool:
		return property.New(x)
	case []any:
		var items *meta.Shape
		if sh != nil && sh.Kind == meta.KindArray {
			items = sh.Items
		}
		out := make([]property.Value, 0, len(x))
		for _, e := range x {
			out = append(out, c.fromJSON(e, items))
		}
		return property.New(out)
	case map[string]any:
		var obj *meta.Object
		var values *meta.Shape
		if sh != nil {
			switch sh.Kind {
			case meta.KindRef, meta.KindUnion:
				obj, _ = c.object(sh)
			case meta.KindMap:
				values = sh.Items
			}
		}
		out := make(map[string]property.Value, len(x))
		for _, k := range sortedKeys(x) {
			name, fieldShape := k, values
			if obj != nil {
				if f, ok := obj.Props[k]; ok {
					name, fieldShape = f.Name, f.Shape
				}
			}
			out[name] = c.fromJSON(x[k], fieldShape)
		}
		return property.New(out)
	}
	return property.New(fmt.Sprint(j))
}

// project narrows an object value to the fields of an object or union
// input shape. It reports false when the value cannot take that shape.
func (c *converter) project(v property.Value, sh *meta.Shape) (property.Value, bool) {
	if sh == nil || !v.IsMap() || (sh.Kind != meta.KindRef && sh.Kind != meta.KindUnion) {
		return v, false
	}
	obj, rev := c.object(sh)
	if obj == nil {
		return v, false
	}
	out := map[string]property.Value{}
	for k, e := range v.AsMap().All {
		api, ok := rev[k]
		if !ok || e.IsNull() {
			continue
		}
		if field := obj.Props[api].Shape; field != nil && (field.Kind == meta.KindRef || field.Kind == meta.KindUnion) && e.IsMap() {
			if e, ok = c.project(e, field); !ok {
				continue
			}
		}
		out[k] = e
	}
	if len(out) == 0 {
		return v, false
	}
	return property.New(out), true
}

// restrict keeps a refreshed value to what the program set: the map entries
// and object fields of prev. An object field the API didn't report keeps its
// previous value; a map entry the API no longer has is dropped, so the diff
// shows it.
func (c *converter) restrict(prev, live property.Value, sh *meta.Shape) property.Value {
	if sh == nil || !prev.IsMap() || !live.IsMap() {
		return live
	}
	lm := live.AsMap()
	out := map[string]property.Value{}
	switch sh.Kind {
	case meta.KindMap:
		for k, pv := range prev.AsMap().All {
			if lv, ok := lm.GetOk(k); ok {
				out[k] = c.restrict(pv, lv, sh.Items)
			}
		}
	case meta.KindRef, meta.KindUnion:
		obj, rev := c.object(sh)
		for k, pv := range prev.AsMap().All {
			lv, ok := lm.GetOk(k)
			if !ok {
				out[k] = pv
				continue
			}
			var field *meta.Shape
			if api, ok := rev[k]; ok && obj != nil {
				field = obj.Props[api].Shape
			}
			out[k] = c.restrict(pv, lv, field)
		}
	default:
		return live
	}
	return property.New(out).WithSecret(live.Secret())
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// plain strips secret markers and dependencies so values compare by content.
func plain(v property.Value) property.Value {
	v = v.WithSecret(false).WithDependencies(nil)
	switch {
	case v.IsArray():
		items := v.AsArray().AsSlice()
		out := make([]property.Value, len(items))
		for i, e := range items {
			out[i] = plain(e)
		}
		return property.New(out)
	case v.IsMap():
		out := map[string]property.Value{}
		for k, e := range v.AsMap().All {
			if e.IsNull() {
				continue
			}
			out[k] = plain(e)
		}
		return property.New(out)
	}
	return v
}

// sameValue reports whether two values are equal, ignoring secrets and
// treating a missing value like null.
func sameValue(a, b property.Value) bool {
	return plain(a).Equals(plain(b))
}
