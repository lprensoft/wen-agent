# Configuration and models

[← Back to README](../../README.en.md)　·　[中文](../configuration.md)　·　English

## First run

A fresh install needs no config file. The first time you run `wen` without a usable model, a terminal wizard comes up first:

1. Pick a language. It is used for the interface, the prompts sent to the model and the runtime log.
2. Pick a model provider. The list is the same preset table as "Add provider" on the settings page (see [Adding a provider](#adding-a-provider) below), with "Custom" as the last entry.
3. Enter the API key and choose a model. Providers pointing at your own machine, such as Ollama, need no key.
4. The connection is tested once. If it fails you can fill it in again, pick another provider, or save it anyway.
5. Confirm the listen port. It defaults to 8080. If another program is using that port, the next free one is suggested instead.

Once you answer, the service starts. To switch models or add providers later, use the settings page or `wen config models`. To go through the wizard again, run `wen onboard`. It adds another provider and switches to it, leaving existing ones alone.

Each answer goes where that kind of setting already lives. No new file is introduced:

- The model goes into `<config dir>/models.json`, the same file the settings page and `wen config` edit.
- The port and language are written only when they **differ from the defaults** and the config directory has no `config.yaml` yet. In that case the wizard creates a `config.yaml` holding just those options. If both are left at their defaults, there is no config file at all.
- The wizard never rewrites an existing `config.yaml`.

When not running in a terminal (systemd, containers, `nohup`), there is nobody to ask. Without a configured model the program prints a hint and exits. For such deployments, run `wen onboard` once in a terminal first, or write the `model:` and `providers:` sections in `config.yaml` as shown below.

In two other cases the wizard does not start even in a terminal. The program reports an error instead, because the wizard cannot fix them:

- The current provider is defined in `config.yaml` but its `api_key` is empty. This almost always means the environment variable behind `${VAR}` is not set, and the error names the provider. The wizard could only add another provider and store a plaintext key in `models.json`.
- An instance using the same config directory is already running. Use `wen onboard` or the settings page instead, and the change takes effect at once through the running instance.

**The config directory** is `~/.wen/` by default. If the current directory contains a `config.yaml`, the current directory is used instead. With `-c`, the directory of the given file is the config directory. The model configuration, sessions, plugin state and access password all live there.

## config.yaml (optional)

You only need it to change the start-up settings below. Put it in the config directory and write just the options you are changing. The rest take their defaults. An existing full `config.yaml` keeps working as before.

| Option | Description |
|---|---|
| `lang` | Which language this service speaks: `zh` / `en`. Left empty it follows the system locale (see below) |
| `server.host` | The listen address, `127.0.0.1` by default. Set an access password before changing it to an external address (see [Deployment and access control](deployment.md)) |
| `server.port` | The listen port, `8080` by default. The `-p` command-line flag takes precedence |
| `server.trust_loopback` | Whether requests from loopback skip authentication, on by default. It **must** be turned off behind a reverse proxy (see [Deployment and access control](deployment.md)) |
| `model.provider` / `model.name` | The provider and model id to start with (the settings page's "Models" section takes over from here; see below). If they are omitted and `providers` has an entry named `deepseek`, it is selected, with the model `deepseek-flash` (the old default `deepseek-v4-flash` has been retired) |
| `model.temperature` | `0.7` by default. It has no effect while thinking is on |
| `model.max_tokens` | The output cap for one reply, `4096` by default |
| `model.thinking` | Thinking effort: `off` / `low` / `medium` / `high` / `xhigh` / `max`, `high` by default (the reasoning is shown as a collapsible block in the web UI) |
| `model.context_length` | The model's context window in tokens, 1000000 by default. Over budget, whole turns are trimmed starting from the oldest; once measured usage reaches 90% of it, the session is compacted into a summary |
| `providers.<name>` | The provider registry: `type` (`openai_compat` / `anthropic`), `base_url`, `api_key`, and optionally `thinking_dialect` and `prompt_cache` (see below) |
| `agent.system_prompt` | The system prompt, empty by default (at runtime a `[system environment]` block — OS, shell, working directory, locale — is injected in front of it) |
| `agent.max_turns` | The cap on tool-loop iterations within one request, `20` by default |
| `agent.workdir` | The working directory shared by the plugins and the environment block; empty means the process's current directory |
| `session.dir` | Where sessions are stored, `<config dir>/sessions` by default |

`model.temperature`, `max_tokens`, `thinking` and `context_length` are also the global defaults. A model entry without its own override falls back to them.

Values may contain `${VAR}`, which is replaced with the environment variable at load time. Deployments without a terminal, such as containers, can configure the model this way instead of using the wizard:

```yaml
model:
  provider: deepseek
  name: deepseek-flash
providers:
  deepseek:
    type: openai_compat
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
```

**Plugin toggles and settings are not in config.yaml.** They are maintained from the settings page and stored only in `<config dir>/plugins.state.json`. On a first run every plugin is enabled with the defaults it declares, except for these thirteen:

- `read_file` and `exec_command` — they can read any file and run any command. You want them when you are using this as an agent that does work for you; turn them on from the settings page, where it is a visible choice.
- `dual_persona` (without trigger phrases there is nothing to switch on), `weather` (it needs a city, and network access), `phone` (it needs feeds, and network access), `skills` (the skills directory is empty, so all it adds is two tools with nothing to do), `bookshelf` (the shelf is empty, so there is nothing to read).
- `qq_bot`, `wechat_bot`, `feishu_bot`, `lark_bot`, `telegram_bot`, `discord_bot` — they need credentials, or a code scanned, before they can do anything.

Everything else is on, **the whole roleplay group and `heartbeat` included** — out of the box it should be what this project actually is; see [Out of the box](plugins/README.md). Heartbeat being on means it spends quota on a fixed rhythm from the moment you install it; turn it off on the settings page if you do not want that. Each plugin's own data lives in `<config dir>/plugins/<plugin>/`.

(Early versions accepted a `plugins:` section in config.yaml. When something can be configured in two places, you have to remember a precedence rule to know which one is in effect — and since the settings page never writes back to the config file, what is written there slowly turns into a lie. That section no longer has any effect; the program says so once at start-up, and you can delete it.)

## Language

`lang:` decides which language **this service** speaks: the prompts and tool descriptions sent to the model, the plugins' status lines, the notes written into sessions, the runtime log, and the output of commands like `wen status` and `wen plugins`. It takes `zh` or `en`; left empty it follows the system locale, and falls back to Chinese when that cannot be read. A language name it does not recognise is treated as if you had left it empty rather than stopping the service. The start-up log and `wen status` each print a line saying which language is in use and whether it was configured or guessed.

If the language you pick in the setup wizard differs from the system locale, the wizard writes it into the `config.yaml` it creates. After that it can only be changed by editing that file — the settings page does not write this one, for the opposite reason to the model settings: `lang` has a single value that almost never changes, and the config file is never written back to (writing back would freeze the plaintext behind `${VAR}` into the file and lose every comment).

**The browser interface's language is a separate thing**, chosen at the top right of the settings page (`Follow browser / 中文 / English`) and stored only in that browser. One service may be open on a computer and a phone in two different languages, while `lang:` is shared — so the two are kept apart: one governs this web page, the other governs what is sent to the model and written to the log.

**Changing the language does not rewrite anything already stored.** Old compaction summaries, memory entries, agenda items and session notes stay in the language they were written in — they are a record of what happened, not interface copy.

## Models (settings page → Models)

Providers and models can be added, edited and removed on the settings page, and the combination in use switched there, with **the next request** picking up the change — no restart.

- Changes are written to `<config dir>/models.json` (mode 0600, already in `.gitignore`) and **never back to config.yaml** — `${VAR}` in an `api_key` is expanded before parsing, so writing back would freeze the plaintext key into the file and lose every comment.
- The `providers:` and `model:` sections of config.yaml, if you wrote them, are initial values: an entry of the same name is fully overridden by models.json, while a provider **newly added** to config.yaml still shows up in the list (marked "from the config file"). Deleting it in the UI records a tombstone, so it does not come back after a restart; an entry you have never touched in the UI is not written to models.json at all and keeps following the config file.
- Each model entry can override `context_length` / `max_tokens` / `thinking` / `temperature` on its own; left empty, the global values from the `model:` section apply.
- **Note**: once a key that came from `${VAR}` has been written into models.json it is fixed there, and changing the environment variable afterwards has no effect.

### Adding a provider

"Add provider" on the settings page opens a list of presets first: pick one and the endpoint, thinking-parameter dialect and common models are all filled in — the only thing left is an API key, and the form links you to the page that issues it. If the vendor you want is not there, the last entry is "Custom" and takes you to the manual form as before. The `wen config` terminal interface follows the same flow.

A preset is only a **set of starting values for the form**, not a third source of configuration: what you save is an ordinary entry of your own, with no further connection to the preset table. A later release that adds a model to the table will not grow one in your entry, and will not change yours back. That is deliberate, for the same reason as the config.yaml / models.json override rule: as soon as two places can configure the same thing, "which one wins" becomes a rule you have to remember.

Where a vendor runs separate accounts inside and outside mainland China (Zhipu, Kimi, Qwen, MiniMax, SiliconFlow), the list carries both, and the China one has `(CN)` in its name. The keys are not interchangeable — a China key will not authenticate against the international host.

The Qwen presets use the two general `dashscope` domains. Alibaba Cloud now recommends workspace-specific domains (`{WorkspaceId}.{region}.maas.aliyuncs.com`). The general domains get no new features after 2026-09-30 but keep working. A workspace domain contains your own workspace ID, so it cannot be a preset. To use one, change the Base URL of that provider to the address your console shows.

The `Ollama` entry points at your own machine and therefore asks for no key; no provider whose `base_url` points at `localhost` / `127.0.0.1` does. It also deliberately ships without models: only your machine knows which ones you have. Run `ollama pull` first, then add the model under that provider. Its thinking-parameter dialect is "send nothing": Ollama answers a thinking parameter sent to a model without thinking support with a 400 (`does not support thinking`), while models that do support thinking think by default without one. If every model you have is a thinking model and you want to set the effort, change this entry's dialect to "generic reasoning_effort".

Model IDs in the presets do expire — vendors retire old models fairly often — so they are reviewed against the changelog at release time. If one stops working, edit it in the UI.

### Thinking-parameter dialects

The OpenAI-compatible protocol only standardises the messages / tools / stream layer. **Thinking parameters are each vendor's own extension** and are mutually incompatible, so a "dialect" is chosen per provider. Presets already pick one; only a custom provider needs you to choose:

| Dialect | On the wire | Levels accepted |
|---|---|---|
| `deepseek` | `thinking:{enabled\|disabled}` + `reasoning_effort` | all five |
| `glm` | same as above | `low` / `high` / `max` |
| `minimax` | `thinking:{adaptive}` + `reasoning_split`; uses `max_completion_tokens` | no levels, and thinking cannot be turned off (`off` is sent as on, because M3.1-Flash rejects `disabled` with a 400) |
| `qwen` | `enable_thinking` boolean | on/off only |
| `kimi` | `reasoning_effort`; no sampling parameters, uses `max_completion_tokens` | `low` / `high` / `max` (`off` is sent as `low`, because the K3 generation cannot turn thinking off) |
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
| `anthropic` | The Anthropic Messages API. Thinking effort maps to `thinking:{type:"adaptive"}` plus `output_config.effort`, with the thinking summary requested (newer models return no thinking text by default). `off` is sent as `disabled` first. If the model rejects it, the error decides the fallback: `between_tools` on Sonnet 5.5, or the lowest effort, `low`, on always-thinking models such as Fable 5.1 and Opus 5.5. On those models "off" still thinks and is still billed, just as little as possible. The working form is remembered for the rest of the process. current-generation Claude models do not accept sampling parameters, so `temperature` is not sent and that field of a model entry has no effect here |

Anthropic mode has one extra switch: **prompt caching** (on by default). With it on, the unchanging parts of a request (tools, system, the history so far) are marked cacheable, and whatever hits the cache is billed at roughly a tenth of the price. The cost is that a miss pays about a quarter extra for the write, and the cache only lives a few minutes — if your conversation moves more slowly than that (chatting on and off through an IM channel, say), turning it off is cheaper. OpenAI-compatible mode has no such switch: caching at DeepSeek and OpenAI is maintained server-side, and whether you hit it depends only on how stable your request prefix is (see [How the context is organized](context.md)).

Anthropic mode sends back thinking blocks from the **current** turn only. Finished turns keep their text and tool calls. Newer models (Fable 5.1, Opus 5.5, Sonnet 5.5) accept a thinking block only if everything before it is unchanged, and this project's history does change between turns: the per-turn status block is attached to that turn's input only, one-time inputs such as the heartbeat prompt are removed afterwards, day markers are added later, and there is compaction. Sending an earlier turn's thinking blocks back would make the whole request fail with a 400. Stripping them is one of the fixes the official docs give. The cost is that Opus 4.5 and later and Sonnet 4.6 and later, which keep earlier turns' thinking by default, no longer see it. Haiku 4.5 and earlier models only ever kept the last turn's thinking. If the prefix changes within the current turn too (budget trimming, or the tool list changing mid-turn), that 400 triggers one automatic retry with every thinking block stripped.
