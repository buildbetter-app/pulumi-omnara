PROJECT    := github.com/buildbetter-app/pulumi-omnara
PROVIDER   := pulumi-resource-omnara
VERSION    ?= 0.1.0
LDFLAGS    := -X $(PROJECT)/provider/pkg/version.Version=$(VERSION)
PULUMI     ?= pulumi
PULUMI_NODE_SDK_VERSION ?= 3.229.0
PULUMI_NODE_SDK_IMPORT_MIDDLE_VERSION ?= 3.5.3
BIN        := provider/bin
OMNARA_REF ?= main

SDK_LANGUAGES := nodejs python go dotnet

.PHONY: all generate provider schema sdks \
	build-sdks build-sdk-nodejs build-sdk-python build-sdk-go build-sdk-dotnet \
	lint e2e e2e-up e2e-down update-spec install clean

all: generate provider sdks

# Regenerate the Pulumi schema and runtime metadata from spec/openapi.yaml.
generate:
	cd provider && go run ./cmd/pulumi-gen-omnara -spec ../spec/openapi.yaml -overlay resources.yaml -out cmd/$(PROVIDER)

provider:
	cd provider && go build -ldflags "$(LDFLAGS)" -o bin/$(PROVIDER) ./cmd/$(PROVIDER)

# The SDKs are generated from the schema the built provider serves, which
# carries VERSION.
schema: provider
	$(PULUMI) package get-schema ./$(BIN)/$(PROVIDER) > $(BIN)/schema.json

sdks: schema
	for lang in $(SDK_LANGUAGES); do \
		rm -rf sdk/$$lang && $(PULUMI) package gen-sdk $(BIN)/schema.json --language $$lang --out sdk || exit 1; \
	done

build-sdks: build-sdk-nodejs build-sdk-python build-sdk-go build-sdk-dotnet

build-sdk-nodejs:
	cd sdk/nodejs && npm install --no-audit --no-fund --package-lock=false --save-exact @pulumi/pulumi@$(PULUMI_NODE_SDK_VERSION) import-in-the-middle@$(PULUMI_NODE_SDK_IMPORT_MIDDLE_VERSION) && npx tsc
	cp README.md LICENSE sdk/nodejs/package.json sdk/nodejs/bin/

build-sdk-python:
	cd sdk/python && python3 -m pip install --quiet build && python3 -m build --outdir ../../dist/python .

build-sdk-go:
	cd sdk && go build ./... && go vet ./...

build-sdk-dotnet:
	cd sdk/dotnet && dotnet build -c Release

lint:
	cd provider && go vet ./...
	cd provider && test -z "$$(gofmt -l .)"

# End-to-end test against a local self-hosted Omnara (needs Docker).
e2e-up:
	e2e/up.sh

e2e: provider e2e-up
	e2e/run.sh

e2e-down:
	e2e/down.sh

# Pull a newer Omnara API spec. Review the diff, then run `make all e2e`.
update-spec:
	curl -fsSL https://raw.githubusercontent.com/omnara-ai/omnara/$(OMNARA_REF)/api/openapi/openapi.yaml -o spec/openapi.yaml
	@echo "Updated spec/openapi.yaml from omnara-ai/omnara@$(OMNARA_REF); record the commit in spec/README.md."

# Put the provider where Pulumi finds local plugins.
install: provider
	cp $(BIN)/$(PROVIDER) $(shell go env GOPATH)/bin/

clean:
	rm -rf $(BIN) dist sdk/nodejs/bin sdk/nodejs/node_modules
