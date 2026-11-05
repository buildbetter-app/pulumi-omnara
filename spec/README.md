# Omnara API specification

`openapi.yaml` is an unmodified copy of `api/openapi/openapi.yaml` from
[omnara-ai/omnara](https://github.com/omnara-ai/omnara), which is licensed under
Apache-2.0.

| Field | Value |
| --- | --- |
| Source commit | [`db8ab4b2510fa7a491eb96820d1114608e5235d8`](https://github.com/omnara-ai/omnara/blob/db8ab4b2510fa7a491eb96820d1114608e5235d8/api/openapi/openapi.yaml) (2026-11-04) |
| Spec version | `0.1.0` (OpenAPI 3.1.2) |
| E2E-tested Omnara release | `v0.2.17` (images pinned in `e2e/compose.yaml`) |

The hosted docs serve the same document at
`https://docs.omnara.com/api-reference/openapi.yaml`, with a header comment added.

## Updating

```sh
make update-spec OMNARA_REF=<commit or tag>
make all          # regenerate schema, metadata, and SDKs
make e2e          # run the provider against a local Omnara
```

Update the commit above. If the spec adds operations that should become
resources or functions, map them in `provider/resources.yaml`. The generator
fails when a mapped operation disappears, stops accepting bearer tokens, or
changes the shape a list read depends on.
