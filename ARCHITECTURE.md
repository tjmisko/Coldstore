# Coldstore Architecture

Coldstore is a daemon that manages cold storage for Syncthing-synced folders. It monitors folder sizes against configurable budgets and offloads old files to a cold storage path, keeping thumbnails and a browsable catalog available via a web UI. A single Go binary embeds both the daemon and a React frontend.

## Quickstart

```bash
git clone <repo-url> && cd coldstore
cp coldstore.toml.example coldstore.toml   # edit to taste
make dev                                    # start daemon (terminal 1)
make dev-web                                # start React dev server (terminal 2)
# open http://localhost:5173
```

## Repo Layout

```
.
├── cmd/coldstore/          # main.go -- binary entry point, CLI flags
├── internal/
│   ├── api/                # HTTP server, REST handlers, embedded React SPA
│   ├── catalog/            # SQLite-backed file catalog (metadata, status tracking)
│   ├── config/             # TOML config parsing and validation
│   ├── daemon/             # core logic: scanner, offloader, restorer, quota enforcement
│   └── thumbnail/          # thumbnail generation pipeline (fan-out worker pool)
├── web/                    # React frontend (Vite + TypeScript)
│   └── src/
│       ├── components/     # UI components (Gallery, FileBrowser, ConfigPanel, etc.)
│       ├── hooks/          # custom hooks (useFiles, useStatus, useFolders, etc.)
│       └── types/          # TypeScript type definitions
├── tests/
│   ├── e2e/                # end-to-end tests (build tag: e2e)
│   └── fixtures/           # test fixture data
├── docker/
│   ├── Dockerfile.test     # test container image
│   └── compose.test.yml    # Docker Compose for live e2e tests with Syncthing
├── docs/                   # detailed documentation (see index below)
├── scripts/                # helper scripts (seed-demo, etc.)
├── coldstore.toml.example  # annotated config template
├── coldstore.service       # systemd unit file
└── Makefile                # build, test, dev targets
```

## Docs Index

| Document | What it covers |
|----------|---------------|
| [Architecture](docs/architecture.md) | System design, data flow, component interactions, API surface |
| [Go Patterns](docs/go-patterns.md) | Annotated walkthrough of Go idioms used in the codebase |
| [React Guide](docs/react-guide.md) | Frontend component tree, custom hooks, infinite scroll, embedding |
| [Build & Test](docs/build-and-test.md) | Makefile targets, build pipeline, test tiers, CI, Docker setup |
| [Deployment](docs/deployment.md) | Ops runbook: config reference, systemd, monitoring, troubleshooting |
| [CLAUDE.md](CLAUDE.md) | AI assistant instructions and project conventions |
