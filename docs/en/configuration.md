# Configuration and models

[← Back to README](../../README.en.md)　·　[中文](../configuration.md)　·　English

## The config file

See [config.example.en.yaml](../../config.example.en.yaml). The essentials:

| Option | Description |
|---|---|
| `lang` | The language of the interface and of everything the program reports: `zh` / `en`. Left empty it follows the system locale (see below) |
| `model.provider` / `model.name` | The provider and model id to start with (the settings page's "Models" section takes over from here; see below) |
| `model.thinking` | Thinking effort: `off` / `low` / `medium` / `high` / `xhigh` / `max`, `high` by default (`temperature` has no effect while thinking is on; the reasoning is shown as a collapsible block in the web UI) |
| `model.context_length` | The model's context window in tokens, 1000000 by default. Over budget, whole turns are trimmed starting from the oldest; once measured usage reaches 90% of it, the session is compacted into a summary |
| `server.trust_loopback` | Whether requests from loopback skip authentication, on by default. It **must** be turned off behind a reverse proxy (see [Deployment and access control](deployment.md)) |
| `providers.<name>` | The provider registry: `type` (`openai_compat` / `anthropic`), `base_url`, `api_key` |
| `agent.system_prompt` | The system prompt (empty by default; at runtime a `[system environment]` block — OS, shell, working directory, locale — is injected in front of it) |
| `agent.max_turns` | The cap on tool-loop iterations within one request |
| `agent.workdir` | The working directory shared by the plugins and the environment block; empty means the process's current directory |
| `session.dir` | Where sessions are stored, `<config dir>/sessions` by default |

**Plugin toggles and settings are not in config.yaml.** They are maintained from the settings page and stored only in `<config dir>/plugins.state.json`. On a first run every plugin is enabled with the defaults it declares, except for these ten:

- `read_file` and `exec_command` — they can read any file and run any command. You want them when you are using this as an agent that does work for you; turn them on from the settings page, where it is a visible choice.
- `dual_persona` (without trigger phrases there is nothing to switch on), `weather` (it needs a city, and network access), `skills` (the skills directory is empty, so all it adds is two tools with nothing to do).
- `qq_bot`, `wechat_bot`, `feishu_bot`, `lark_bot`, `telegram_bot` — they need credentials, or a code scanned, before they can do anything.

Everything else is on, **the whole roleplay group and `heartbeat` included** — out of the box it should be what this project actually is; see [Out of the box](plugins/README.md). Heartbeat being on means it spends quota on a fixed rhythm from the moment you install it; turn it off on the settings page if you do not want that. Each plugin's own data lives in `<config dir>/plugins/<plugin>/`.

(Early versions accepted a `plugins:` section in config.yaml. When something can be configured in two places, you have to remember a precedence rule to know which one is in effect — and since the settings page never writes back to the config file, what is written there slowly turns into a lie. That section no longer has any effect; the program says so once at start-up, and you can delete it.)

## Language

`lang:` decides which language **this service** speaks: the prompts and tool descriptions sent to the model, the plugins' status lines, the notes written into sessions, the runtime log, and the output of commands like `wen status` and `wen plugins`. It takes `zh` or `en`; left empty it follows the system locale, and falls back to Chinese when that cannot be read. A language name it does not recognise is treated as if you had left it empty rather than stopping the service. The start-up log and `wen status` each print a line saying which language is in use and whether it was configured or guessed.

It can only be changed by editing config.yaml — the settings page does not write this one, for the opposite reason to the model settings: `lang` has a single value that almost never changes, and the config file is never written back to (writing back would freeze the plaintext behind `${VAR}` into the file and lose every comment).

**The browser interface's language is a separate thing**, chosen at the top right of the settings page (`Follow browser / 中文 / English`) and stored only in that browser. One service may be open on a computer and a phone in two different languages, while `lang:` is shared — so the two are kept apart: one governs this web page, the other governs what is sent to the model and written to the log.

**Changing the language does not rewrite anything already stored.** Old compaction summaries, memory entries, agenda items and session notes stay in the language they were written in — they are a record of what happened, not interface copy.

## Models (settings page → Models)

Providers and models can be added, edited and removed on the settings page, and the combination in use switched there, with **the next request** picking up the change — no restart.

- Changes are written to `<config dir>/models.json` (mode 0600, already in `.gitignore`) and **never back to config.yaml** — `${VAR}` in an `api_key` is expanded before parsing, so writing back would freeze the plaintext key into the file and lose every comment.
- The `providers:` and `model:` sections of config.yaml are initial values: an entry of the same name is fully overridden by models.json, while a provider **newly added** to config.yaml still shows up in the list (marked "from the config file"). Deleting it in the UI records a tombstone, so it does not come back after a restart; an entry you have never touched in the UI is not written to models.json at all and keeps following the config file.
- Each model entry can override `context_length` / `max_tokens` / `thinking` / `temperature` on its own; left empty, the global values from the `model:` section apply.
- **Note**: once a key that came from `${VAR}` has been written into models.json it is fixed there, and changing the environment variable afterwards has no effect.

### Adding a provider

"Add provider" on the settings page opens a list of presets first: pick one and the endpoint, thinking-parameter dialect and common models are all filled in — the only thing left is an API key, and the form links you to the page that issues it. If the vendor you want is not there, the last entry is "Custom" and takes you to the manual form as before. The `wen config` terminal interface follows the same flow.

A preset is only a **set of starting values for the form**, not a third source of configuration: what you save is an ordinary entry of your own, with no further connection to the preset table. A later release that adds a model to the table will not grow one in your entry, and will not change yours back. That is deliberate, for the same reason as the config.yaml / models.json override rule: as soon as two places can configure the same thing, "which one wins" becomes a rule you have to remember.

Where a vendor runs separate accounts inside and outside mainland China (Zhipu, Kimi, Qwen, MiniMax, SiliconFlow), the list carries both, and the China one has `(CN)` in its name. The keys are not interchangeable — a China key will not authenticate against the international host.

The `Ollama` entry points at your own machine and therefore asks for no key; no provider whose `base_url` points at `localhost` / `127.0.0.1` does. It also deliberately ships without models: only your machine knows which ones you have. Run `ollama pull` first, then add the model under that provider.

Model IDs in the presets do expire — vendors retire old models fairly often — so they are reviewed against the changelog at release time. If one stops working, edit it in the UI.

### Thinking-parameter dialects

The OpenAI-compatible protocol only standardises the messages / tools / stream layer. **Thinking parameters are each vendor's own extension** and are mutually incompatible, so a "dialect" is chosen per provider. Presets already pick one; only a custom provider needs you to choose:

| Dialect | On the wire | Levels accepted |
|---|---|---|
| `deepseek` | `thinking:{enabled\|disabled}` + `reasoning_effort` | all five |
| `glm` | same as above | `low` / `high` / `max` |
| `minimax` | `thinking:{adaptive\|disabled}` + `reasoning_split` | on/off only |
| `qwen` | `enable_thinking` boolean | on/off only |
| `kimi` | `reasoning_effort` | `low` / `high` / `max` |
| `openai` | `reasoning_effort`; no sampling parameters, uses `max_completion_tokens` | all six (`off` is sent as `none`) |
| `gemini` | `reasoning_effort` | `low` / `medium` / `high` |
| `xai` | `reasoning_effort` | `low` / `medium` / `high` / `xhigh` |
| `effort` | `reasoning_effort` | all five, sent as-is |
| `none` | no thinking parameters sent | — |

The six levels in the UI (`off` / `low` / `medium` / `high` / `xhigh` / `max`) come from Anthropic, and no other vendor accepts all of them. When you pick a level the provider does not have, it is **rounded up**: the lowest level that is not below your choice, or the highest one available if there is none. Someone who picks `medium` wants "a bit more thought than low", and giving them low gives less than asked. So `medium` under the `glm` dialect goes out as `high`, and `max` under `gemini` goes out as `high`.

One case rounding cannot save: `glm-5.3` always thinks, and setting the level to "off" makes it fail outright. The preset pins `high` on that model — leave it alone.

Two API modes are supported:

| Mode | Description |
|---|---|
| `openai_compat` | The OpenAI-compatible protocol (DeepSeek and others) |
| `anthropic` | The Anthropic Messages API. Thinking effort maps to `thinking:{type:"adaptive"}` plus `output_config.effort` (`off` becomes `disabled`); current-generation Claude models do not accept sampling parameters, so `temperature` is not sent and that field of a model entry has no effect here |

Anthropic mode has one extra switch: **prompt caching** (on by default). With it on, the unchanging parts of a request (tools, system, the history so far) are marked cacheable, and whatever hits the cache is billed at roughly a tenth of the price. The cost is that a miss pays about a quarter extra for the write, and the cache only lives a few minutes — if your conversation moves more slowly than that (chatting on and off through an IM channel, say), turning it off is cheaper. OpenAI-compatible mode has no such switch: caching at DeepSeek and OpenAI is maintained server-side, and whether you hit it depends only on how stable your request prefix is (see [How the context is organized](context.md)).
