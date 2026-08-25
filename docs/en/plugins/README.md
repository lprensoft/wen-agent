# Plugin overview

[← Docs index](../README.md)　·　[← Back to the project README](../../../README.en.md)　·　[中文](../../plugins/README.md)　·　English

The core (agent / session / server / llm) contains no concrete tools; every capability comes from a plugin. Plugin toggles and settings are maintained on the web UI's settings page (or with `wen config plugins` on a remote machine), take effect immediately, and are never written to config.yaml.

The editable prompts among those settings (the heartbeat prompt, the fallback line and so on) **come with defaults in the interface language**: under an English interface the textarea is pre-filled with the English version, and "restore defaults" returns to it. The fixed prompts a plugin injects into the model are still written in Chinese.

| Group | Plugin | In one line | Default |
|---|---|---|---|
| [Basic tools](tools.md) | `read_file` | Reads a local text file | on |
| | `web_fetch` | Fetches the text of a web page | on |
| | `exec_command` | Runs commands, intercepting destructive ones for a human to confirm | on |
| [Memory and search](memory.md) | `memory` | Long-term memory across sessions: automatic distillation, revision on contradiction, a daily timeline, optional forgetting | on |
| | `session_search` | Looks past conversations up by keyword and date, including the full pre-compaction archives | on |
| | `skills` | Skill manuals (SKILL.md) read on demand | off |
| [Roleplay](roleplay.md) | `roleplay` | Character sheet, sample lines, natural expression and time-consistency rules | off |
| | `dual_persona` | An outer and an inner persona switched by trigger phrases, with memory and history invisible across them | off |
| [The character's life](life.md) | `scene` | The stage setting and scene memories | off |
| | `weather` | Real weather by city, injected each turn as ambient state | off |
| | `belongings` | What is in the fridge and the wardrobe | off |
| | `people` | The character's social circle: friends, family and closeness | off |
| | `agenda` | A plan it makes for itself each day and goes and does; a ledger of commitments and promises | off |
| | `relationship` | The stage you are at, what you call each other, understandings and off-limits subjects | off |
| | `unspoken` | What it has not said out loud | off |
| | `body_sense` | Per-part familiarity with touch, plus arousal and fatigue | off |
| | `mood` | A mood moved by interaction and drifting back over time | off |
| | `health` | The occasional everyday ailment, rising and falling over its course | off |
| | `presence` | Where it is right now, what it is wearing, how it is sitting, what it is doing | off |
| | `style_watch` | Measures how often assistant-speak shows up, using regex rules | off |
| [Background work](background.md) | `heartbeat` | Speaks first on an interval, with the character setting its own pace | off |
| | `scheduler` | Scheduled tasks (one-off / recurring / daily) | on |
| [Chat channels](im.md) | `qq_bot` | The official QQ open platform | off |
| | `wechat_bot` | The official WeChat ClawBot plugin (bind by QR code) | off |
| | `feishu_bot` | Feishu (China) | off |
| | `lark_bot` | Lark (international) | off |
| | `telegram_bot` | The Telegram Bot API | off |
| [Maintenance](self-update.md) | `self_update` | Checks for a new release, then downloads, verifies, replaces and restarts in one click | on |
| [External plugins](external.md) | `bundles` | Install Agent Plugins bundles | on |
| | `ext_*` | One card per installed bundle | on |

To write one yourself, see [Project layout and writing plugins](../architecture.md).
