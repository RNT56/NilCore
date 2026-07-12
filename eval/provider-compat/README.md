# Provider compatibility evaluation

This hermetic suite exercises the Phase 15 provider wire contract against local
`httptest.Server` fixtures. It covers:

- a generic OpenAI-compatible endpoint with a non-default prefix and auth scheme;
- reasoning-model token caps (`max_completion_tokens`, never both cap keys);
- strict `json_schema` structured output;
- OpenRouter routing, fallbacks, reasoning, transforms, plugins, and attribution;
- native web-search rendering and the I7 boundary for provider and client search.

Run it with:

```sh
go test ./eval/provider-compat -v
```

The suite never contacts a live provider. Each adapter call is served by a local
fixture, and the client-side search path uses an in-memory sandbox double. Golden
transcripts in `testdata/` make request-shape drift reviewable.

Native provider search and client fallback have different safe data paths. Raw
Anthropic server-tool blocks are dropped during decode, so attacker-controlled
snippets never re-enter the loop. Client-side results must remain visible to the
model, so they are injection-flagged for audit and unconditionally fenced as
untrusted data.
