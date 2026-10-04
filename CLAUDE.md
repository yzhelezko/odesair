# odesair

Telegram userbot (MTProto user account via `gotd/td`, not a bot token). An LLM agent reads Odesa news channels in real time, around the clock, and decides on its own when to post an alert to the output channel, by calling a tool.

Single long-running Go process, flat `package main`. No DB, no inbound HTTP, no CLI flags.

## Commands

- `make run` — `source .env && go run .`
- `make build` — binary to `bin/odesair`
- `make test` — `go vet` + `go test -race`
- `make bench` — agent and intake benchmarks
- `make live` — real LLM turns against the provider in `.env`, Telegram faked (`LIVE_LLM=1`, a few paid calls)
- `make login` — ChatGPT sign-in in the browser; writes the token file for `LLM_MODEL=openai/...`
- CI (`.github/workflows/ci.yaml`): vet + race tests, then build and push `ghcr.io/yzhelezko/odesair/odesair` (`latest` + short sha, linux/amd64). PR builds push too.

## Flow

```
push updates ─┐
5s poll ──────┼─► Intake (dedup) ─► Agent inbox ─► LLM + tools ─► Telegram
siren poller ─┘     (alert events go straight to the inbox)
```

| File | Role |
|---|---|
| `main.go` | Wiring, signals, goroutines under one `errgroup` |
| `config.go` | Env parsing and validation |
| `telegram.go` | Auth, one-time peer resolve, update handler, `Send`, `Recent` |
| `intake.go` | Merges push and poll, drops empty, stale and duplicate posts |
| `siren.go` | Polls the air-alert API, tracks the alert window, emits alert start/end as posts from `система` |
| `agent.go` | Inbox loop, turn, history, context budget, prompt file reload |
| `tools.go` | `send_alert`, `get_recent_messages`, send guards, alert memory |
| `llm.go` | OpenAI-compatible chat completions client with tool calling |
| `responses.go` | Responses API client for the ChatGPT backend (SSE, tool calling) |
| `openai_auth.go` | ChatGPT sign-in (OAuth with PKCE), token file, refresh |

- The push watcher (`updates.Manager`) feeds the agent all the time; it only delivers channels the account has joined (`push=true` in the startup log).
- At startup the account joins every source it is not a member of yet. A source that cannot be joined stays on the poll.
- The poll always covers channels without push. Joined channels are polled too during the alert window (alert plus `ALERT_GRACE`), as a safety net for pushes that lag.
- The agent runs a turn the moment a post arrives. Posts that arrive during an LLM call form the next turn; there is no batch timer.
- A turn ends without a follow-up LLM call when the only tool calls were accepted `send_alert`s.
- No tool call means nothing is posted. Guards on `send_alert`: length cap, duplicate text, rate cap.
- `danger=true` posts `🚨` with sound, `danger=false` posts `✅` silently.

## Invariants

- **Cache prefix.** Each request must extend the previous one byte for byte: static system prompt, static tools, history stored exactly as sent (including the model's reasoning: `reasoning_content` on chat completions, encrypted reasoning items on the Responses API), all per-turn state in the newest user message. History is trimmed rarely and in one step (down to 60% of the budget), then the next turn restates sent alerts. Do not put time or other dynamic data in the system prompt.
- **No duplicate alerts on retry.** A turn is retried only if the LLM failed before any alert was sent.
- **Context budget** is in tokens; bytes-per-token is calibrated from the provider's reported `prompt_tokens`.

## Config

| Var | Default | Notes |
|---|---|---|
| `APPID`, `APPHASH`, `PHONE_NUMBER` | — | Required. Telegram credentials |
| `TG_PASSWORD` | empty | 2FA password, only for first login |
| `API_KEY` | — | LLM key. Not needed with ChatGPT sign-in |
| `LLM_BASE_URL` | `https://api.z.ai/api/coding/paas/v4` | `/chat/completions` is appended |
| `LLM_MODEL` | `glm-5.3` | `openai/<id>` with no `LLM_BASE_URL` selects ChatGPT sign-in and the Responses API |
| `LLM_EFFORT` | `medium` | Reasoning effort; empty omits it |
| `OPENAI_AUTH_FILE` | `config/openai-auth.json` | Token file written by `make login`; must be writable |
| `LLM_CONTEXT_TOKENS` | `32000` | System prompt + history |
| `SOURCE_CHANNELS` | `xydessa_live,freechat_odesa,odesairxydessa,Sila_GO` | Comma-separated usernames |
| `SEND_TO_CHANNEL` | `odesair` | Output channel |
| `ENABLE_TELEGRAM_SEND` | `true` | `false` = dry run: alerts are logged, not posted |
| `ALERT_GRACE` | `10m` | Alert window extension after the alert ends |
| `LOG_LEVEL` | `info` | |

A set-but-empty variable does not fall back to its default.

Runtime files, relative to cwd, all gitignored:

- `config/system_message.txt` — system prompt (Russian): danger criteria and the numbered rules for when to call `send_alert`. Re-read when its mtime changes. Startup fails without it.
- `config/tdlib-session` — Telegram session. Without it the login code is read from stdin.
- `.env`, `bin/` — hold real secrets (`bin/adesair.yaml` is a k8s manifest with plaintext env and the session Secret). Never print or commit their contents.

Deploy: k8s Deployment, 1 replica; container cwd is `/`, so config mounts at `/config/...`.

## Gotchas

- Running locally uses the same Telegram session as the deployed pod. Two clients on one auth key can get the session revoked (`AUTH_KEY_DUPLICATED`); stop the pod first or ask before running.
- GLM `reasoning_effort`: 5.3 accepts only `low`/`high`/`max` and cannot disable thinking; 5.2 maps `low`/`medium` to `high` and skips thinking on `none`/`minimal`.
- Z.ai's subscription terms restrict the Coding Plan endpoint to supported coding tools; a bot calling it directly may be throttled or cut off.
- ChatGPT sign-in: the token is refreshed from 3 days before it expires. If refreshing keeps failing, alerts posted in the last 2 days carry a "token expires in 2 days / 1 day" line; once it expires the agent stops until a new `make login`.
- ChatGPT sign-in: the refresh token rotates on every refresh and the new one is written back to the token file. One login must live in one place only; a second copy goes stale and its refresh fails. A replaced token file is picked up without a restart.
- ChatGPT sign-in uses the Codex OAuth client and the ChatGPT backend with a subscription, outside the intended coding use; OpenAI may restrict the account.
- Via OpenRouter, cache hits depend on which upstream serves the call; a miss there does not mean the prefix changed.
- Text-only: media in posts is ignored.
- Posts come from public channels and one public chat, so they are untrusted input to an agent that can post publicly; the prompt tells the model to treat them as data.
