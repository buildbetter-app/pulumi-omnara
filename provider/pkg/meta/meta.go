// Package meta describes how each Pulumi resource and function in the Omnara
// provider maps onto Omnara REST operations. The generator writes it next to
// the Pulumi schema, and the provider runtime embeds it.
package meta

// Metadata is the generated runtime contract for the provider.
type Metadata struct {
	Resources map[string]*Resource `json:"resources"`
	Functions map[string]*Function `json:"functions"`
	// Types maps a Pulumi type token to the shape of its object.
	Types map[string]*Object `json:"types"`
}

// Resource describes one Pulumi resource.
type Resource struct {
	Create  *Operation   `json:"create"`
	Read    *Operation   `json:"read,omitempty"`
	Updates []*Operation `json:"updates,omitempty"`
	Delete  *Operation   `json:"delete,omitempty"`

	// Inputs and Outputs map Pulumi property names to their metadata.
	Inputs  map[string]*Property `json:"inputs"`
	Outputs map[string]*Property `json:"outputs"`

	// IDParam names the path parameter that holds the resource's own ID.
	// It is empty for resources whose ID is composed from IDProps.
	IDParam string `json:"idParam,omitempty"`
	// IDProps lists the Pulumi input properties joined with "/" to form a
	// composite ID when the API has no single identifier for the resource.
	IDProps []string `json:"idProps,omitempty"`
	// ScopeProps lists, in path order, the Pulumi inputs that locate the
	// resource (for example orgId, projectId). Import IDs carry these values
	// before the resource ID.
	ScopeProps []string `json:"scopeProps,omitempty"`
	// UniqueProps lists inputs that the API keeps unique, so a replacement
	// that keeps their values must delete the old resource first.
	UniqueProps []string `json:"uniqueProps,omitempty"`
	// DeleteBeforeReplace always deletes the old resource first, for APIs
	// whose create returns an existing matching object.
	DeleteBeforeReplace bool `json:"deleteBeforeReplace,omitempty"`
	// InputFromOutput maps an input to the output that reports its live
	// value when the API echoes it under another name.
	InputFromOutput map[string]string `json:"inputFromOutput,omitempty"`
	// ServerKeys lists, per map input, keys that the API adds itself and
	// that reads must not copy into inputs.
	ServerKeys map[string][]string `json:"serverKeys,omitempty"`
}

// Operation is one HTTP call.
type Operation struct {
	OperationID string `json:"operationId"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	// PathParams maps each path parameter to the Pulumi property that fills
	// it, or to "$id" for the resource's own ID.
	PathParams map[string]string `json:"pathParams,omitempty"`
	// Query maps query parameter names to Pulumi properties. For resource
	// reads the property may be a dotted path into an object input.
	Query map[string]string `json:"query,omitempty"`
	// QueryDefaults fills query parameters that no property supplies, such
	// as during an import, when there are no inputs yet.
	QueryDefaults map[string]string `json:"queryDefaults,omitempty"`
	// Body lists the Pulumi input properties sent in the request body.
	Body []string `json:"body,omitempty"`
	// BodyEncoding is "json", "multipart", or "raw".
	BodyEncoding string `json:"bodyEncoding,omitempty"`
	// Nullable lists body properties that the API clears when sent as null.
	Nullable []string `json:"nullable,omitempty"`
	// FromState maps API body fields to Pulumi output properties of the
	// current state, for values the update needs but the user doesn't set.
	FromState map[string]string `json:"fromState,omitempty"`
	// Result is the response field that holds the resource object, or empty
	// when the whole response body is the object.
	Result string `json:"result,omitempty"`
	// Extra maps other response fields to Pulumi output properties.
	Extra map[string]string `json:"extra,omitempty"`
	// List marks a read that must page through a collection.
	List bool `json:"list,omitempty"`
	// Match maps item fields to Pulumi properties ("$id" for the ID) that
	// must all match when searching a list.
	Match map[string]string `json:"match,omitempty"`
	// GoneIfSet names a response field whose non-null value means the
	// resource no longer exists (for example revoked_at).
	GoneIfSet string `json:"goneIfSet,omitempty"`
	// Paged marks list operations that take limit and cursor parameters.
	Paged bool `json:"paged,omitempty"`
	// Idempotent marks operations that accept an Idempotency-Key header.
	Idempotent bool `json:"idempotent,omitempty"`
	// Paginate aggregates every page of a list into Items (functions only).
	Paginate bool `json:"paginate,omitempty"`
}

// Function describes one Pulumi invoke.
type Function struct {
	Operation *Operation           `json:"operation"`
	Inputs    map[string]*Property `json:"inputs"`
	Outputs   map[string]*Property `json:"outputs"`
	// ItemsProp names the output that holds aggregated list items.
	ItemsProp string `json:"itemsProp,omitempty"`
}

// Property maps a Pulumi property onto an API field.
type Property struct {
	API     string `json:"api"`
	Shape   *Shape `json:"shape"`
	Secret  bool   `json:"secret,omitempty"`
	Replace bool   `json:"replace,omitempty"`
	// Scope marks inputs that come from the request path rather than the body.
	Scope bool `json:"scope,omitempty"`
	// DefaultFromConfig names a provider config key used when the input is unset.
	DefaultFromConfig string `json:"defaultFromConfig,omitempty"`
	// Computed marks outputs that only the API sets.
	Computed bool `json:"computed,omitempty"`
}

// Object is a named object type's field mapping.
type Object struct {
	// Props maps API field names to Pulumi names and shapes.
	Props map[string]*Field `json:"props"`
}

// Field is one property of an Object.
type Field struct {
	Name  string `json:"name"`
	Shape *Shape `json:"shape"`
}

// Shape kinds.
const (
	KindPrimitive = "prim"
	KindAny       = "any"
	KindRef       = "ref"
	KindArray     = "array"
	KindMap       = "map"
	KindUnion     = "union"
	KindArchive   = "archive"
	KindAsset     = "asset"
)

// Shape describes a value so the runtime can convert property names between
// Pulumi's camelCase and the API's snake_case without touching map keys.
type Shape struct {
	Kind  string `json:"k"`
	Ref   string `json:"ref,omitempty"`
	Items *Shape `json:"items,omitempty"`
	// Variants lists the object type tokens a union may hold.
	Variants []string `json:"variants,omitempty"`
}
