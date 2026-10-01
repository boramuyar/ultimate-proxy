# Contributing

Thanks for helping. Bug reports, fixes and small improvements are welcome as issues and pull requests.

## Scope

Omni Proxy is a pass-through proxy for OpenAI-compatible APIs plus control (who may call, which
models, how much) and accounting (tokens, cost, cache health). It deliberately does not translate
between APIs, route or retry between models, or rewrite prompts. Please open an issue before
building something outside that scope, so we can talk about it first.

## Making a change

```sh
docker compose up -d --build    # the whole stack, with a free fake provider
go test ./...                   # unit and integration tests, no provider keys needed
cd web && npm ci && npm run dev # the dashboard on http://localhost:5173
```

The "Develop" section of the README lists the tests that need Postgres, ClickHouse or Redis. Before
you open a pull request:

- `gofmt -l .` prints nothing and `go vet ./...` passes.
- New behavior has a test, and the README describes anything a user sees.
- User-visible changes get a line under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md).

CI runs the tests against Postgres, ClickHouse, Valkey, Redis and Dragonfly, the Open Responses
compliance suite, and the whole compose stack.

## Releases

Versions follow [Semantic Versioning](https://semver.org/). To release:

1. Rename `## [Unreleased]` in CHANGELOG.md to the new version and date, add an empty
   `## [Unreleased]` above it, update the links at the bottom, and merge that to `main`.
2. Tag the merge commit and push the tag: `git tag v0.2.0 && git push origin v0.2.0`.

The Release workflow then publishes the images to ghcr.io and creates the GitHub release with that
version's changelog section as its notes.
