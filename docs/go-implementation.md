# Symphony Go Implementation

This repository includes a Go implementation of the Symphony service described by
[`SPEC.md`](../SPEC.md).

## Status

The Go implementation is an engineering preview for trusted environments. It implements the core
daemon shape:

- `WORKFLOW.md` loading with YAML front matter and strict prompt rendering
- typed config defaults, `$VAR` indirection, path normalization, and dynamic reload
- GitLab issue reads for candidates, terminal cleanup, and running-state refresh
- per-issue workspace creation, sanitized paths, containment checks, and lifecycle hooks
- opencode local-server or external-server sessions over HTTP, with SSE event parsing
- polling orchestration, bounded concurrency, retries, stall detection, reconciliation, and cleanup
- structured JSON logs
- optional HTTP observability endpoints under `/api/v1/*`

The optional `gitlab_api` opencode tool extension and SSH worker extension are not implemented in
the Go service yet.

## Run

```bash
go run ./cmd/symphony ./WORKFLOW.md
```

Useful flags:

```bash
go run ./cmd/symphony --port 8080 --logs-root ./log ./WORKFLOW.md
```

- `--port` enables the dashboard and JSON API on loopback by default.
- `--logs-root` also writes structured logs to `symphony.log` in that directory.
- Without a positional path, the CLI uses `./WORKFLOW.md`.

## Minimal Workflow

```md
---
tracker:
  kind: gitlab
  api_key: $GITLAB_TOKEN
  project_id: "123456"
  required_labels: ["symphony"]
workspace:
  root: ~/code/symphony-workspaces
hooks:
  after_create: |
    git clone git@gitlab.com:your-group/your-project.git .
agent:
  max_concurrent_agents: 2
  max_turns: 3
opencode:
  command: opencode serve --hostname "$OPENCODE_HOST" --port "$OPENCODE_PORT"
---

You are working on GitLab issue {{ issue.identifier }}.

Title: {{ issue.title }}
Description: {{ issue.description | default: "No description provided." }}
```

Set `GITLAB_TOKEN` before starting the service. If `opencode.base_url` is configured, Symphony uses
that already-running server instead of launching `opencode.command`.

## Safety Posture

This implementation is intended for trusted operator-controlled environments. Workspace path
containment is enforced, and opencode is launched with the per-issue workspace as its working
directory. Hook scripts and opencode permissions are still trusted workflow configuration.

`opencode.config` is materialized into `.opencode/symphony-config.json` inside the issue workspace
before a local opencode server is launched. When the workspace does not already contain a repo-owned
`opencode.json`, Symphony also writes the same pass-through payload to `opencode.json` so the local
server can load it as native project config. Existing `opencode.json` files are not overwritten; in
that case the sidecar remains available for an opencode-side wrapper, plugin, or future config API
integration. The `opencode.permission` alias is folded into the generated payload when
`opencode.config.permission` is not already present, and model/agent/permission fields are also sent
in message requests where the targeted opencode API accepts them. Broader tool/MCP pass-through setup
depends on the deployed opencode version and is not implemented as a built-in Symphony tool
installer.

When `opencode.base_url` points at an already-running external server, Symphony does not launch that
server and cannot independently enforce that the external server is rooted in the per-issue
workspace. Use external servers only when their own deployment enforces equivalent workspace
isolation.

Permission requests or user-input-required signals should be configured in opencode so runs do not
wait indefinitely. For stricter deployments, add host-level isolation such as a dedicated OS user,
container, VM, network restrictions, and narrower credentials.

## Test

```bash
GOCACHE=/tmp/symphony-gocache go test ./...
```
