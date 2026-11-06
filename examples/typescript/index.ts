import * as pulumi from "@pulumi/pulumi";
import * as omnara from "@buildbetter-app/pulumi-omnara";

const config = new pulumi.Config();
// Set the provider with `pulumi config set omnara:orgId org_...` and
// `pulumi config set --secret omnara:apiKey omnara_pat_v1_...`.
const anthropicApiKey = config.requireSecret("anthropicApiKey");
const modelBaseUrl = config.get("modelBaseUrl");

const project = new omnara.Project("harness", { name: `agent-harness-${pulumi.getStack()}` });

// Model access: an org secret holds the API key; the project gets a grant.
const anthropicKey = new omnara.Secret("anthropic-key", {
    name: `anthropic-api-key-${pulumi.getStack()}`,
    owner: { kind: "org" },
    material: { kind: "generic", value: anthropicApiKey },
});

const anthropic = new omnara.ModelProviderConfig("anthropic", {
    name: `anthropic-${pulumi.getStack()}`,
    credentialSecretId: anthropicKey.id,
    ...(modelBaseUrl ? { apiFormat: "anthropic-messages", baseUrl: modelBaseUrl } : { preset: "anthropic" }),
});

const sonnet = new omnara.ConfiguredModel("sonnet", {
    modelProviderConfigId: anthropic.id,
    name: "sonnet",
    providerModelSlug: config.get("modelSlug") ?? "claude-sonnet-5-5",
    contextWindowTokens: 200_000,
    supportsTools: true,
});

const sonnetGrant = new omnara.ProjectModelGrant("sonnet", {
    projectId: project.id,
    configuredModelId: sonnet.id,
});

// Agent resources: a skill uploaded from a directory and a memory store.
const triageNotes = new omnara.Skill("triage-notes", {
    owner: { kind: "project", projectId: project.id },
    archive: new pulumi.asset.FileArchive("./skills/triage-notes"),
});

const memory = new omnara.MemoryStore("triage-memory", {
    projectId: project.id,
    name: "triage-memory",
    description: "What the triage agent learned about recurring reports.",
    agentAccess: "read_write",
});

// The agent itself. A typed definition compiles to the same config as YAML.
const triageConfig = new omnara.AgentConfig("triage", {
    projectId: project.id,
    sourceFormat: "json",
    source: {
        version: "v1",
        instruction: "Triage new bug reports. Follow the triage-notes skill and record recurring patterns in memory.",
        model: {
            providerConfig: anthropic.name,
            name: sonnet.name,
        },
        tools: {
            web_search: {},
            ask_question: {},
        },
        skills: [triageNotes.id],
        memoryStores: [{ name: memory.name, access: "read_write" }],
    },
}, { dependsOn: [sonnetGrant] });

const triage = new omnara.AgentProfile("triage", {
    projectId: project.id,
    name: "triage",
    configId: triageConfig.id,
});

new omnara.CronTrigger("weekday-triage", {
    projectId: project.id,
    name: "weekday-triage",
    cron: "0 9 * * 1-5",
    timezone: "America/New_York",
    enabled: false,
    messageTemplate: "Triage reports filed since the last run.",
    target: { type: "profile", agentProfileId: triage.id },
});

export const projectId = project.id;
export const triageProfileId = triage.id;
export const triageConfigId = triage.currentConfigId;
export const skillRevision = triageNotes.revision;
