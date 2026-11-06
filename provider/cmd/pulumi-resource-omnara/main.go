// Command pulumi-resource-omnara is the Omnara Pulumi provider plugin.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"

	p "github.com/pulumi/pulumi-go-provider"

	"github.com/buildbetter-app/pulumi-omnara/provider/pkg/provider"
	"github.com/buildbetter-app/pulumi-omnara/provider/pkg/version"
)

//go:embed schema.json
var schema []byte

//go:embed metadata.json
var metadata []byte

func main() {
	prov, err := provider.New(version.Version, schema, metadata)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := p.RunProvider(context.Background(), provider.Name, version.Version, prov); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
