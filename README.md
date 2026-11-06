# Pulumi provider for Omnara

Manage [Omnara](https://omnara.com) with Pulumi. Omnara is the open-source managed agent platform. This provider covers projects, secrets, model providers, machines and machine pools, skills, memory stores, agent configs and profiles, cron triggers, event webhooks, and integrations. It works against hosted Omnara (`https://api.omnara.com/v1`) and self-hosted deployments.

The provider is generated from Omnara's [OpenAPI specification](spec/openapi.yaml). A small mapping file, [`provider/resources.yaml`](provider/resources.yaml), says which operations make up each resource. A generic runtime then performs the REST calls. When Omnara's API changes, most updates only need the spec refreshed and the code regenerated.

> **Status:** 0.1.0. Omnara's public API is itself at version 0.1.0 and still changing, so expect breaking changes in both.

## Install

### Provider plugin

Each `v*` tag publishes plugin binaries to this repository's [releases](https://github.com/buildbetter-app/pulumi-omnara/releases). Pulumi downloads them automatically through the schema's `pluginDownloadURL`. To build the plugin from source instead:

```sh
make provider install   # builds provider/bin/pulumi-resource-omnara and copies it to $GOPATH/bin
```

### SDKs

| Language | Package |
| --- | --- |
| YAML | Nothing to install. Reference `omnara:index:<Resource>` types. |
| TypeScript/JavaScript | `@buildbetter-app/pulumi-omnara`. Install the `.tgz` attached to a release: `npm install https://github.com/buildbetter-app/pulumi-omnara/releases/download/v0.1.0/buildbetter-app-pulumi-omnara-0.1.0.tgz` |
| Python | `pulumi_omnara`: `pip install "git+https://github.com/buildbetter-app/pulumi-omnara@v0.1.0#subdirectory=sdk/python"` |
| Go | `go get github.com/buildbetter-app/pulumi-omnara/sdk/go/omnara` (module `github.com/buildbetter-app/pulumi-omnara/sdk`) |
| .NET | `BuildBetter.Omnara`. Use the `.nupkg` attached to a release. |

## Configure

| Setting | Environment variable | Description |
| --- | --- | --- |
| `omnara:apiKey` (secret) | `OMNARA_API_KEY` | A personal access token (`omnara_pat_v1_…`) or an organization API key. Create either in the Omnara dashboard. |
| `omnara:apiUrl` | `OMNARA_API_URL` | Defaults to `https://api.omnara.com/v1`. For self-hosted Omnara, use `<origin>/api/v1`. |
| `omnara:orgId` | `OMNARA_ORG_ID` | Default organization for resources and functions that don't set `orgId`. |

```sh
pulumi config set omnara:orgId org_...
pulumi config set --secret omnara:apiKey omnara_pat_v1_...
```

Organization API keys can't call user-only endpoints such as `getCurrentUser`.

## Example

```typescript
import * as pulumi from "@pulumi/pulumi";
import * as omnara from "@buildbetter-app/pulumi-omnara";

const project = new omnara.Project("harness", { name: "agent-harness" });

const key = new omnara.Secret("anthropic-key", {
    name: "anthropic-api-key",
    owner: { kind: "org" },
    material: { kind: "generic", value: new pulumi.Config().requireSecret("anthropicApiKey") },
});

const anthropic = new omnara.ModelProviderConfig("anthropic", {
    name: "anthropic", preset: "anthropic", credentialSecretId: key.id,
});
const sonnet = new omnara.ConfiguredModel("sonnet", {
    modelProviderConfigId: anthropic.id, name: "sonnet",
    providerModelSlug: "claude-sonnet-5-5", contextWindowTokens: 200_000,
});
const grant = new omnara.ProjectModelGrant("sonnet", { projectId: project.id, configuredModelId: sonnet.id });

const config = new omnara.AgentConfig("triage", {
    projectId: project.id,
    sourceFormat: "json",
    source: {
        version: "v1",
        instruction: "Triage new bug reports.",
        model: { providerConfig: anthropic.name, name: sonnet.name },
        tools: { web_search: {} },
    },
}, { dependsOn: [grant] });

new omnara.AgentProfile("triage", { projectId: project.id, name: "triage", configId: config.id });
```

[`examples/typescript`](examples/typescript) adds a skill, a memory store, and a cron trigger. [`examples/yaml`](examples/yaml) builds the same setup in Pulumi YAML. `AgentConfig.source` also accepts YAML text, for example read from an `agent.yaml` file.

## Resources

| Resource | Notes |
| --- | --- |
| `Project` | Renaming replaces the project. |
| `OrgMemberProjectAccess` | Gives an existing org member a project role. ID: `<userId>/<projectId>`. |
| `Secret`, `SecretGrant` | Org, project, or user secrets. `material` is write-only: it stays in Pulumi state as a secret, and a change uploads a new version. |
| `ModelProviderConfig`, `ConfiguredModel`, `ProjectModelGrant` | Model access. Fields that can't be updated (such as `apiFormat`) force a replacement. |
| `Machine`, `MachineDaemonToken`, `ProjectMachineGrant` | Bring-your-own machines. The daemon token is only returned on create and is stored as a secret. Deleting the token resource revokes the token. |
| `MachinePool`, `ProjectMachinePoolGrant` | Sandbox-provider pools. Each provider requires different capacity fields; see the resource docs. |
| `Skill`, `SkillGrant` | `archive` is a directory or archive with `SKILL.md` at its root, or one directory that contains it. A change uploads a new revision. Renaming the skill in `SKILL.md` replaces it. |
| `AgentConfig` | Immutable. Any change creates a new config. Omnara has no delete for configs, so destroying one only removes it from the stack. |
| `AgentProfile` | Changing `configId` moves the profile to the new config. Renaming happens in place. |
| `CronTrigger`, `EventWebhook` | Webhook imports find project-scoped webhooks only. |
| `Integration`, `IntegrationSubscription` | OAuth and app-installation setup flows still run in the dashboard. A subscription is deleted before it is replaced, because creating a duplicate returns the existing one. |
| `MemoryStore` | |

Functions: `getCurrentUser`, `getOrganizations`, `getProjects`, `getProjectSecrets`, `getProjectSkills`, `getProjectMachines`, `getToolCatalog`, `getModelCatalog`, `getIntegrationDefinitions`, `getAgentConfig`, `getAgentProfile`. List functions return every page in `items`.

### Not covered

- **Org API keys and personal access tokens.** Omnara only lets a dashboard session manage them, not a bearer token. The generator refuses operations that don't accept bearer tokens.
- **Organizations and invitations.** Deleting an org is too destructive to tie to a stack, and an accepted invitation disappears.
- **Agent runs, inputs, events, and interactions.** These are runtime activity, not infrastructure. Use the Omnara SDK or API.
- **Memory store files, actors, and integration setup flows.**

## Behavior

- **Import.** IDs carry the path scope: `<orgId>/<projectId>/<id>` for project resources and `<orgId>/<id>` for org resources. The leading `orgId` may be left out when the provider sets `orgId`. For example: `pulumi import omnara:index:Secret llm-key sec_...`. Omnara never returns write-only values (`Secret.material`, `Skill.archive`). Either add `ignoreChanges` for them on the first `up` after an import, or let that `up` upload them again.
- **Updates and replacements.** A field the API can update changes in place. Any other field forces a replacement. A replacement that keeps a unique `name` deletes the old object first, so the names don't collide.
- **Removing an optional field.** A removed map or list is cleared, and a removed nullable field is set to null. A removed field the API can't clear keeps its live value, and the preview doesn't show it as a change.
- **Refresh** reads live state. It updates only what your program sets: inputs, map entries, and object fields. Server-side defaults and keys the API adds (such as `observed_platform` on machine metadata) don't show up as diffs. Values changed, removed, or cleared to null outside Pulumi do, and the next `up` restores them.
- **Secrets** stay secret in state at any depth. A secret inside a map, such as one `Machine.env` value, remains encrypted even though the API returns it in plain text.

## How it's built

```
spec/openapi.yaml ──┐
                    ├─ pulumi-gen-omnara ─▶ schema.json + metadata.json ─▶ pulumi-resource-omnara
resources.yaml ─────┘                              │
                                                   └─▶ pulumi package gen-sdk ─▶ sdk/{nodejs,python,go,dotnet}
```

- `provider/pkg/gen` maps OpenAPI 3.1 schemas onto Pulumi types. It handles `$ref`, `allOf`, discriminated `oneOf`, nullable types, maps, and binary uploads, and converts snake_case to camelCase. Pulumi's schema binder validates the result.
- `provider/pkg/provider` serves the embedded schema. It implements check, diff, create, read, update, delete, import, and invoke from the metadata. Names are converted using the shapes, so map keys stay as written.
- Most API changes need `make update-spec all` and a review of the diff. A new resource needs an entry in `resources.yaml`.

## Development

| Command | What it does |
| --- | --- |
| `make generate` | Regenerate `schema.json` and `metadata.json` from the spec and overlay. |
| `make provider` | Build `provider/bin/pulumi-resource-omnara`. |
| `make sdks` | Regenerate every SDK. Uses the Pulumi CLI version pinned in CI. |
| `make build-sdks` | Compile the SDKs. |
| `make e2e` | Start a local self-hosted Omnara with Docker and run the end-to-end suite against it. |
| `make e2e-down` | Remove the local Omnara and its volumes. |
| `make update-spec OMNARA_REF=<ref>` | Pull a newer spec from `omnara-ai/omnara`. |

The end-to-end suite ([`e2e/run.sh`](e2e/run.sh)) runs a real self-hosted Omnara release (pinned in [`e2e/compose.yaml`](e2e/compose.yaml)). It creates every resource, then checks that preview and refresh find no changes. Next it makes an out-of-band edit and an out-of-band delete, and checks that Pulumi detects and repairs both. It then applies in-place updates and replacements, and imports objects by every ID form, both with `pulumi import` and with the `import` resource option. Last, it destroys the stack and confirms through the API that the objects are gone. Each run writes a transcript of every command and its exit code to `e2e/.state/runs/<timestamp>/transcript.txt`. CI runs the same suite.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
