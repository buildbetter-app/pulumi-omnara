// Command pulumi-gen-omnara generates the provider's Pulumi schema and
// runtime metadata from the Omnara OpenAPI document.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/blang/semver"
	pschema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"

	"github.com/buildbetter-app/pulumi-omnara/provider/pkg/gen"
)

func main() {
	specPath := flag.String("spec", "spec/openapi.yaml", "Omnara OpenAPI document")
	overlayPath := flag.String("overlay", "provider/resources.yaml", "resource mapping")
	outDir := flag.String("out", "provider/cmd/pulumi-resource-omnara", "directory for schema.json and metadata.json")
	version := flag.String("version", "", "package version to stamp into the schema (omit for the embedded schema)")
	flag.Parse()
	if err := run(*specPath, *overlayPath, *outDir, *version); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(specPath, overlayPath, outDir, version string) error {
	spec, err := gen.LoadSpec(specPath)
	if err != nil {
		return err
	}
	overlay, err := gen.LoadOverlay(overlayPath)
	if err != nil {
		return err
	}
	res, err := gen.Generate(spec, overlay, version)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if _, diags, err := pschema.BindSpec(res.Schema, noDeps{}, pschema.ValidationOptions{}); err != nil {
		return fmt.Errorf("bind schema: %w", err)
	} else if diags.HasErrors() {
		return fmt.Errorf("schema is invalid: %s", diags.Error())
	}
	if err := writeJSON(filepath.Join(outDir, "schema.json"), res.Schema); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outDir, "metadata.json"), res.Metadata); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "generated %d resources, %d functions, %d types\n",
		len(res.Schema.Resources), len(res.Schema.Functions), len(res.Schema.Types))
	return nil
}

func writeJSON(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// noDeps is a schema loader for a package with no package dependencies.
type noDeps struct{}

func (noDeps) LoadPackage(pkg string, _ *semver.Version) (*pschema.Package, error) {
	return nil, fmt.Errorf("unexpected package dependency %q", pkg)
}

func (noDeps) LoadPackageV2(_ context.Context, d *pschema.PackageDescriptor) (*pschema.Package, error) {
	return nil, fmt.Errorf("unexpected package dependency %q", d.Name)
}
