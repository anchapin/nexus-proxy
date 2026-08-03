# Agent Integration Guides

Nexus Proxy exposes an OpenAI-compatible `/v1/chat/completions` endpoint.
Any agent that supports a custom OpenAI base URL can use the proxy — set the
base URL to `http://localhost:8000/v1` and supply any non-empty API key.

> **Note:** The proxy authenticates inbound traffic via `NEXUS_PROXY_API_KEY`
> (or passes all requests when that variable is empty). The API key your agent
> sends is *not* the provider key — it is checked against the proxy's own gate.
> Use any non-empty placeholder string if the proxy has no inbound key set.

## Quick Reference

| Agent      | Config path                                    | Base URL                             | API key field            |
| ---------- | ---------------------------------------------- | ------------------------------------ | ------------------------ |
| Cursor     | Settings → Models → OpenAI API Key             | `http://localhost:8000/v1`           | OpenAI API Key field     |
| Windsurf   | Settings → Models → Add Custom Model            | `http://localhost:8000/v1`           | API Key field            |
| Cline      | VS Code Settings → `cline.apiKey` / `cline.baseUrl` | `http://localhost:8000/v1`   | `cline.apiKey` in settings |
| Continue   | `~/.continue/config.yaml` → `models[].apiBase`  | `http://localhost:8000/v1`           | `models[].apiKey`        |

## Cursor

1. Open Cursor settings: **Settings → Models** (or click the model selector
   in the top-right of the chat panel and select **Settings**).
2. Click **Add Model** → **OpenAI API Key**.
3. In the **Base URL** field, enter:

   ```
   http://localhost:8000/v1
   ```

4. In the **API Key** field, enter any non-empty string (e.g.
   `nexus-proxy`).
5. Click **Save**. Select the model name you normally use (e.g.
   `gpt-4o`). The model name is passed straight through to the proxy, which
   routes it based on its own pipeline.

> **Tip:** Cursor sends all requests to the configured base URL. The proxy's
> routing pipeline decides whether to serve locally (Ollama) or escalate to
> the frontier — you do not need separate configurations for local vs.
> remote models.

## Windsurf

1. Open Windsurf settings: **Settings → Models**.
2. Click **Add Custom Model** (or edit an existing OpenAI-compatible entry).
3. Set the **Base URL** to:

   ```
   http://localhost:8000/v1
   ```

4. Set the **API Key** to any non-empty string (e.g. `nexus-proxy`).
5. Click **Save**. Select the custom model for your chat session.

> **Note:** Windsurf's config is also stored in
> `~/.codeium/windsurf/config.json` if you prefer to edit it directly. Look
> for the `customModels` or `openai` section and set `baseUrl` and `apiKey`
> accordingly.

## Cline (VS Code)

1. Open VS Code Settings (**Ctrl+,** / **Cmd+,**).
2. Search for `cline.baseUrl` and set it to:

   ```
   http://localhost:8000/v1
   ```

3. Search for `cline.apiKey` and set it to any non-empty string (e.g.
   `nexus-proxy`).
4. Alternatively, open Cline's chat panel, click the gear icon
   (model provider settings), and select **OpenAI Compatible**. Enter the
   base URL and API key in the UI.
5. Select a model name (e.g. `gpt-4o`). The name passes through to the proxy
   unchanged.

> **Note:** Cline sends the model name in the request body as-is. The proxy
> applies its routing pipeline (DSL → SLM → frontier escalation) regardless
> of the requested model.

## Continue.dev

1. Open (or create) the Continue config file:

   ```
   ~/.continue/config.yaml
   ```

2. Add or modify a model entry:

   ```yaml
   models:
     - title: Nexus Proxy
       provider: openai
       model: gpt-4o
       apiBase: http://localhost:8000/v1
       apiKey: nexus-proxy
   ```

3. Save the file. Restart Continue if it is already running.
4. Select **Nexus Proxy** from the model picker in the Continue sidebar.

> **Tip:** You can add multiple entries pointing at the same proxy but with
> different `model` values (e.g. `gpt-4o` and `claude-3-5-sonnet`). The proxy
> passes the model name through and routes based on its own pipeline, so
> each name gets the same cost-optimization treatment.

## Model Passthrough

All four agents send the requested model name in the request body (e.g.
`"model": "gpt-4o"`). Nexus Proxy does **not** require you to register model
names — the value is passed through to the routing pipeline, which decides
the optimal route (local, frontier, or fusion) regardless of what name the
agent requested.

If you have model aliases configured (`NEXUS_MODEL_ALIASES`), the proxy can
rewrite the model name to a specific provider/model pair before routing.

## Troubleshooting

| Symptom | Likely cause | Fix |
| ------- | ------------ | --- |
| `401 Unauthorized` | `NEXUS_PROXY_API_KEY` is set but the agent sends a different key | Set the agent's API key field to match `NEXUS_PROXY_API_KEY`, or unset `NEXUS_PROXY_API_KEY` to disable inbound auth |
| `Connection refused` | Proxy is not running | Start the proxy: `./bin/nexus` or `make build && ./bin/nexus` |
| Requests always go to frontier | Ollama is down or circuit breaker is tripped | Check Ollama: `ollama list`. Run `nexus check` to verify the health poller status |
| Streaming hangs | Agent does not support SSE over the configured transport | Ensure the agent's base URL uses `http://` (not `https://`) for local proxy |
