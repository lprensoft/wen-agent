<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="internal/server/webui/assets/logo-lockup-dark.png">
    <img src="internal/server/webui/assets/logo-lockup-light.png" alt="Wen Agent" width="300">
  </picture>
</p>

<p align="center"><b>An AI character who lives a life of their own — and stays the same person while doing it.</b></p>

<p align="center"><a href="README.md">中文</a>　·　English</p>

An AI persona with a life of its own. Written in Go, deployed as a single binary; the idea comes from [project-sue-](https://github.com/lzh830913-wq/project-sue-) by Liu Zhehong (刘哲宏). It is not aiming at "good answers" but at two other things. **Aliveness** — the character has its own day and its own social circle, catches a cold now and then, keeps things it never said out loud, starts a conversation when you have been quiet for a while, and texts like a person in your chat apps. **A personality that stays put** — it is not warm one turn and distant the next; after the history is compacted, or in a brand-new session, it is still the same person. Every trait and every piece of state lives in a plugin you can see, toggle and configure, instead of being improvised by the model each turn.

The goal is **someone you can keep company with**, not a window that only exists while it is open. The core keeps only general mechanisms; every concrete capability is a plugin: an embedded web UI, long-term memory across sessions, local session storage, and five chat channels — QQ, WeChat, Feishu, Lark and Telegram. Turn the roleplay plugins off and it falls back to an ordinary get-things-done agent.

## Where the name comes from

"Wen" comes from Su Jingwen (苏静雯) — the first AI persona built and lived with under these ideas. Much of what is in this project (what a character should remember, what it should sound like when it speaks up after a few days of silence) became clear through that company. The project is named after the last character of her name so as not to forget her.

## Features

- **[Roleplay](docs/en/plugins/roleplay.md)** — **a character ships with it** (Wen, twenty-nine, a marketing planner in Shanghai): install it, start talking, and that is who answers, with nothing to configure; rewrite the sheet on the settings page if she is not what you wanted. The character sheet, sample lines and "about me" are injected as top-priority prompts; replies open with a 【】 block describing the scene, the action and the expression. A set of natural-expression rules suppresses the mechanical register Chinese LLM output tends to fall into, and time-consistency rules keep the season, the light outside and phrases like "last time" lined up with the real clock. The character may refuse in its own voice, and when a provider blocks a call the failure is translated into the character drifting off mid-sentence rather than a technical error dropped into the conversation
- **[A character with state](docs/en/plugins/life.md)** — presence, scenes, belongings, people, agenda, relationship, unspoken thoughts, body, mood, health, weather: each kept by its own plugin, surviving compaction and new sessions. It cooks only with what is actually in the fridge, mentions only friends who exist in its people book, plans two to four things for itself each day and goes and does them, keeps promises in a ledger with a due date and a settled state — and when it rains where the character lives, it really is raining there
- **[Two personas, outer and inner](docs/en/plugins/roleplay.md)** — switched by trigger phrases. The inner persona's conversations and memories are completely invisible to the outer one: not just the contents, but the counts and titles too. The two sides can each own a chat channel — say the phrase on QQ and the side that takes over answers you on WeChat
- **[Memory that remembers, and forgets](docs/en/plugins/memory.md)** — a cross-session store of facts whose index is injected every turn, with relevant entries surfacing on their own when you touch on them; distilled periodically during a conversation and once more before the history is compacted; a new conclusion that contradicts an old memory revises it in place; each day's conversation is folded into a one-line timeline the next day; gradual forgetting is available. The raw conversations stay searchable by keyword and date through [history search](docs/en/plugins/memory.md), archives of pre-compaction transcripts included
- **[It speaks first — and knows when not to](docs/en/plugins/background.md)** — a heartbeat wakes the model up on the most recently active session, and the character sets the timing of its next opening itself: closer when the conversation is lively, paused until morning when you say you are going to bed. Things like rain that has just started become a reason to speak. Scheduled tasks can be created mid-conversation and run in the background, writing their results back into the session
- **[Remote channels](docs/en/plugins/im.md)** — QQ, WeChat, Feishu, Lark and Telegram, each mapping a remote user to an ordinary session, with `/new` `/status` `/compact` `/help` and remote confirmation of dangerous operations; replies produced by background turns are pushed to the remote side
- **[Plugin architecture](docs/en/architecture.md)** — a small core (agent loop / session / server / llm) plus plugins; twenty-nine built-in plugins grouped by function, toggled and configured from the settings page, effective immediately. The core knows only general mechanisms — it has never heard of "memory", "persona" or "mood"
- **[Configurable models](docs/en/configuration.md)** — providers and models are added, edited and switched from the settings page and take effect on the next request; OpenAI-compatible and Anthropic Messages API are both supported
- **[A human in the loop for commands](docs/en/plugins/tools.md)** — `exec_command` intercepts before execution and asks you; the most destructive commands are refused outright. No path sandbox that could be trivially bypassed
- **Web UI** — a single-page chat interface embedded with `go:embed`, SSE streaming, visible thinking and tool calls; background activity shows up in the session in real time
- **[It updates itself](docs/en/plugins/self-update.md)** — checks GitHub for a new release once a day; one click on the settings page downloads, verifies (SHA256), test-runs, replaces and restarts. No package manager needed on Windows, macOS or Linux. It only checks — installing is always your click
- **Session management** — one JSONL file per session (a meta line followed by one line per message); history survives restarts
- **One config file** — everything including API keys lives in `config.yaml` (already in .gitignore), and values support `${VAR}` from the environment

## Quick start

Prebuilt binaries are on the [Releases](https://github.com/lprensoft/project-wen/releases) page (windows / linux / darwin × amd64 / arm64, with SHA256). The one tagged `dev` is a rolling prerelease whose version reads like `v0.6.0-3-gxxxxxxx` — "the 3rd commit after the last stable release". Or build it yourself:

```bash
# 1. Config: copy the example and fill in the api_key of the provider you use
#    (the example uses providers.deepseek)
cp config.example.en.yaml config.yaml

# 2. Run (or go build -o wen ./cmd/wen and run ./wen)
go run ./cmd/wen

# 3. Open your browser
# http://127.0.0.1:8080
```

There is nothing to configure first: nineteen of the twenty-nine plugins are on by default — the character, memory, agenda, mood and heartbeat among them — so the other side is someone with a personality, a life, and the habit of speaking first. The ten that are off either need something filled in (trigger phrases, a city, bot credentials) or carry too much power: `read_file` can read any file and `exec_command` can run any command, which is what you want when you are using this as an agent that does work for you. Turn those on from the settings page.

The browser interface's language is chosen at the top right of the settings page. **The prompts sent to the model, the log and the command-line output follow `lang:` in config.yaml**, which falls back to the system locale when unset (see [Configuration and models](docs/en/configuration.md#language)).

Config lookup order: the path given to `-c` → `config.yaml` in the current directory → `~/.wen/config.yaml`.

```bash
wen -c /path/to/config.yaml -p 9000   # explicit config and port
```

The main commands (see [CLI](docs/en/cli.md) for the full list):

```
wen                        start the server (same as wen serve)
wen config [section]       guided configuration (sections: plugins / models / server)
wen status                 print the current configuration and runtime state
wen eval <script.json>     replay a scripted conversation, report style and consistency
wen update [--check] [-y]  check for and install a new version
```

## Configuration

`config.yaml` holds only start-up configuration — model, server, sessions. The essentials are in [Configuration and models](docs/en/configuration.md); the full example is [config.example.en.yaml](config.example.en.yaml).

**Plugin toggles and settings are not in config.yaml.** They are maintained from the settings page (or `wen config plugins` on a remote machine) and stored only in `<config dir>/plugins.state.json`; each plugin's own data lives in `<config dir>/plugins/<plugin>/`.

Before deploying to a remote server, read [Deployment and access control](docs/en/deployment.md). The web UI listens on `127.0.0.1` only by default. `exec_command` and `read_file` are off by default, but once you turn them on from the settings page, unauthorized access means more than someone reading your chat logs — those two run any command and read any file.

## Plugins

Twenty-nine built-in plugins, grouped by function, all toggleable and configurable from the settings page. Per-plugin documentation and settings are in the [plugin overview](docs/en/plugins/README.md).

| Group | Plugins |
|---|---|
| [Basic tools](docs/en/plugins/tools.md) | `read_file` `web_fetch` `exec_command` |
| [Memory and search](docs/en/plugins/memory.md) | `memory` `session_search` `skills` |
| [Roleplay](docs/en/plugins/roleplay.md) | `roleplay` `dual_persona` |
| [The character's life](docs/en/plugins/life.md) | `scene` `weather` `belongings` `people` `agenda` `relationship` `unspoken` `body_sense` `mood` `health` `presence` `style_watch` |
| [Background work](docs/en/plugins/background.md) | `heartbeat` `scheduler` |
| [Chat channels](docs/en/plugins/im.md) | `qq_bot` `wechat_bot` `feishu_bot` `lark_bot` `telegram_bot` |
| [Maintenance](docs/en/plugins/self-update.md) | `self_update` |
| [External bundles](docs/en/plugins/external.md) | `bundles` |

## Documentation

- [CLI](docs/en/cli.md) — the subcommands, and how to change plugin settings on a headless machine
- [Configuration and models](docs/en/configuration.md) — every option in config.yaml, and provider/model management
- [Plugin overview](docs/en/plugins/README.md) — what each of the twenty-nine plugins does, with its settings, and which are on out of the box
- [Deployment and access control](docs/en/deployment.md) — the authentication model, a start/stop script, and systemd
- [Replay evaluation (wen eval)](docs/en/evaluation.md) — turning "does the character feel better after that prompt change?" into a repeatable script
- [How the context is organized](docs/en/context.md) — what one request is made of, and why the current time is not in the system message
- [HTTP API](docs/en/http-api.md) — the endpoints shared by the web UI and any external program
- [Project layout and writing plugins](docs/en/architecture.md) — a tour of the tree, the `Plugin` interface and its optional companions, and visibility scopes

## Development

```bash
go build ./...   # build
go test ./...    # test
go vet ./...     # vet
```

## License

Released under the [MIT License](LICENSE).

## Roadmap

- More provider types (Ollama and friends) and model fallback
- Auto-updating external plugin bundles, and verifying where they came from
