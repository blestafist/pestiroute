# Claude Code candidate fixture provenance

Every fixture is generated locally and synthetic. None is a provider/client
capture, protocol confirmation, authorization, or evidence of live
interoperability. OAuth values use reserved `.invalid` hosts and unmistakable
placeholders; no credentials, account data, private prompts, or proprietary
artifacts are included. Shapes are testing candidates informed by
`docs/implementation/M7-BINDING.md` and the source-only research in
`docs/references/CLAUDE_CODE_CONNECTOR_RESEARCH.md`. Fixture success is not
provider proof; capabilities remain Unknown and the connector is non-routable.

`manifest.json` is the versioned inventory. `TestFixturesIntegrity` verifies
the SHA-256 digest of every listed fixture, rejects unlisted files, and checks
the fixture-specific parsing/invariant coverage.
