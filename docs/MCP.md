# MCP server for AI agents

paas exposes a [Model Context Protocol](https://modelcontextprotocol.io) server at `POST /mcp`, so
AI agents (Claude Code, Claude Desktop, Cursor, any MCP client) can look at your apps, read their
logs and deploy, roll back, restart or scale them.

- Transport: **Streamable HTTP** (JSON responses; no server-sent event stream). Protocol revision
  `2025-06-18`; clients asking for `2025-03-26` or `2024-11-05` get that revision.
- Methods: `initialize`, `notifications/initialized`, `ping`, `tools/list`, `tools/call`.
- Auth: `Authorization: Bearer <API token>`. Create one in the dashboard (Settings → API tokens) or
  with `POST /api/v1/tokens`. Session tokens are not accepted.

## Connect a client

**Claude Code**

```sh
claude mcp add --transport http paas https://paas.example.com/mcp \
  --header "Authorization: Bearer paas_xxxxxxxxxxxxxxxx"
```

Then ask, for example, "why did the last deploy of `api` in project `shop` fail?" or "roll back `web`".

**Claude Desktop** (`claude_desktop_config.json`), through the `mcp-remote` bridge:

```json
{
  "mcpServers": {
    "paas": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "https://paas.example.com/mcp", "--header", "Authorization:${AUTH_HEADER}"],
      "env": { "AUTH_HEADER": "Bearer paas_xxxxxxxxxxxxxxxx" }
    }
  }
}
```

**Other clients**: point any client that supports the Streamable HTTP transport at
`https://<dashboard domain>/mcp` with the `Authorization` header above. A quick check with curl:

```sh
curl -s https://paas.example.com/mcp -H "Authorization: Bearer $PAAS_TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## Tools

| Tool | Kind | What it does |
|---|---|---|
| `list_projects` | read | your teams (with your role) and their projects |
| `list_services` | read | services of a project: type, status, URL, running instances |
| `get_service` | read | status, source, size, instances, current and recent deployments, domains, linked databases |
| `list_deployments` | read | recent deployments of a service |
| `get_deployment` | read | one deployment: status, commit, timing, error |
| `get_build_logs` | read | last *N* lines (default 200, max 2000) of a deployment's build/rollout log, redacted |
| `get_runtime_logs` | read | last *N* lines of every running instance's output, redacted |
| `get_metrics` | read | CPU, memory, request rate and 5xx summary over the last hours |
| `list_env_names` | read | names of the environment variables (never values) |
| `list_databases` | read | databases of a project: engine, version, mode, size, status (no credentials) |
| `list_nodes` | read | cluster nodes and usage (platform administrators) |
| `deploy_service` | write | build and deploy the branch head or a given commit |
| `rollback_service` | write | redeploy an earlier deployment (the previous one by default) |
| `restart_service` | write | rolling restart |
| `scale_service` | write | set replicas (1–50) |
| `set_env` | write, destructive | set and/or remove variables, then restart; values are never echoed |
| `diagnose_deployment` | AI | ask the configured model to explain a failure (see below) |

Every tool has a JSON Schema `inputSchema` and `annotations` (`readOnlyHint`, `destructiveHint`,
`idempotentHint`), so clients can ask before write tools run.

## Security model

- **Same permissions as the REST API.** Each tool call is executed by dispatching the matching
  REST request internally with the caller's token, so team membership and roles are checked by
  the very same handlers: viewers can read, developers and above can deploy, roll back, restart,
  scale and change environment variables; other teams' projects are invisible; `list_nodes` needs a
  platform administrator.
- **No credentials.** There are no tools to create tokens, read environment variable values,
  database passwords, webhook secrets, 2FA material or node join tokens. Logs and errors pass
  through a redactor that removes the service's secret values (its environment, linked database
  URLs, git token) and anything that looks like a credential (URL passwords, bearer tokens,
  `*_PASSWORD=`/`*_TOKEN=` assignments, private keys, well-known key formats).
- **Audit.** Every write tool call, allowed or refused, is written to the audit log as
  `mcp.<tool>` with the target service (names only, never values).
- **Rate limit.** 120 requests per minute per token (429 with `Retry-After` beyond that); batches
  hold at most 20 messages.
- Requests with a foreign `Origin` header are refused (DNS rebinding protection).

Use a dedicated token per agent and revoke it in Settings → API tokens when done. The agent acts
as you: give it a token of an account with the smallest role it needs (a viewer account for
read-only use).

## AI failure diagnosis

Independent of MCP, an administrator can set up a language model in **Settings → AI**
(`PUT /api/v1/admin/ai`), off by default:

- **Anthropic**: API key (stored encrypted with the master key), model (default
  `claude-sonnet-5-5`).
- **OpenAI-compatible**: any `/chat/completions` endpoint, e.g. Ollama, vLLM or LiteLLM on your own
  hardware (`http://ollama.internal:11434/v1`), with a model name and an optional key.

Failed deployments then get an **Explain this failure** button in the dashboard, `paas diagnose
<service|deployment>` in the CLI and the `diagnose_deployment` tool. The platform sends the tail
of the build log, the app's output, the error (including out-of-memory kills), the detected stack
and the service's size, all redacted and capped at 32 KB, and returns
`{summary, likely_cause, suggested_fixes[]}`. Nothing is applied automatically. Answers are cached
per deployment; developers and above can ask (30 per user per hour), everyone in the team can read
the cached answer.
