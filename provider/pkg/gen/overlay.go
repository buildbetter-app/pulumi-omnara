package gen

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Overlay is provider/resources.yaml. It picks the operations that make up
// each resource and function; the spec supplies everything else.
type Overlay struct {
	Package    PackageInfo                 `yaml:"package"`
	PathParams map[string]string           `yaml:"pathParams"`
	Resources  map[string]*ResourceOverlay `yaml:"resources"`
	Functions  map[string]*OpRef           `yaml:"functions"`
}

// PackageInfo holds package-level schema fields.
type PackageInfo struct {
	Name              string   `yaml:"name"`
	DisplayName       string   `yaml:"displayName"`
	Description       string   `yaml:"description"`
	Publisher         string   `yaml:"publisher"`
	Homepage          string   `yaml:"homepage"`
	Repository        string   `yaml:"repository"`
	License           string   `yaml:"license"`
	Keywords          []string `yaml:"keywords"`
	PluginDownloadURL string   `yaml:"pluginDownloadURL"`
}

// ResourceOverlay configures one resource.
type ResourceOverlay struct {
	Description string            `yaml:"description"`
	Create      *OpRef            `yaml:"create"`
	Read        *OpRef            `yaml:"read"`
	Update      []*OpRef          `yaml:"update"`
	Delete      *OpRef            `yaml:"delete"`
	Rename      map[string]string `yaml:"rename"`
	Secret      []string          `yaml:"secret"`
	SkipInputs  []string          `yaml:"skipInputs"`
	SkipOutputs []string          `yaml:"skipOutputs"`
	AnyOutputs  []string          `yaml:"anyOutputs"`
	ID          []string          `yaml:"id"`
	Unique      *[]string         `yaml:"unique"`
	// DeleteBeforeReplace always deletes the old resource before creating
	// its replacement.
	DeleteBeforeReplace bool `yaml:"deleteBeforeReplace"`
	// ReadInputs maps inputs to the outputs that report their live values.
	ReadInputs map[string]string `yaml:"readInputs"`
	// ServerKeys lists keys the API adds to map inputs.
	ServerKeys map[string][]string `yaml:"serverKeys"`
}

// OpRef names an operation and how to use it.
type OpRef struct {
	Op            string            `yaml:"op"`
	Result        string            `yaml:"result"`
	Extra         map[string]string `yaml:"extra"`
	FromState     map[string]string `yaml:"fromState"`
	Match         map[string]string `yaml:"match"`
	GoneIfSet     string            `yaml:"goneIfSet"`
	Query         map[string]string `yaml:"query"`
	QueryDefaults map[string]string `yaml:"queryDefaults"`
	Paginate      bool              `yaml:"paginate"`
}

// UnmarshalYAML accepts a bare operationId or the object form.
func (r *OpRef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Op = value.Value
		return nil
	}
	type plain OpRef
	return value.Decode((*plain)(r))
}

// LoadOverlay reads resources.yaml.
func LoadOverlay(path string) (*Overlay, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var o Overlay
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&o); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &o, nil
}
