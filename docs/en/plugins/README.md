# Plugin overview

[← Docs index](../README.md)　·　[← Back to the project README](../../../README.en.md)　·　[中文](../../plugins/README.md)　·　English

The core (agent / session / server / llm) contains no concrete tools; every capability comes from a plugin. Plugin toggles and settings are maintained on the web UI's settings page (or with `wen config plugins` on a remote machine), take effect immediately, and are never written to config.yaml.

Everything a plugin sends to the model — the prompts it injects, the block names, the tool descriptions and their parameters — **follows `lang:`** (see [Configuration and models](../configuration.md#language)). That includes the editable prompts among the settings (the heartbeat prompt, the fallback line and so on): under English the textarea is pre-filled with the English version, and "restore defaults" returns to it.

**Out of the box you get a character, not an assistant.** Everything except the thirteen marked "off" below is enabled, and `roleplay` ships with a character sheet and sample lines of its own — start talking without changing a setting and the other side is someone with a personality, a life, and the habit of speaking first. Rewrite the sheet or turn the group off if that is not what you want.

| Group | Plugin | In one line | Default |
|---|---|---|---|
| [Basic tools](tools.md) | `read_file` | Reads a local text file | off |
| | `web_fetch` | Fetches the text of a web page | on |
| | `exec_command` | Runs commands, intercepting destructive ones for a human to confirm | off |
| [Memory and search](memory.md) | `memory` | Long-term memory across sessions: automatic distillation, revision on contradiction, a daily timeline, optional forgetting | on |
| | `session_search` | Looks past conversations up by keyword and date, including the full pre-compaction archives | on |
| | `skills` | Skill manuals (SKILL.md) read on demand | off |
| [Roleplay](roleplay.md) | `roleplay` | Character sheet, sample lines, natural expression and time-consistency rules | on |
| | `dual_persona` | An outer and an inner persona switched by trigger phrases, with memory and history invisible across them | off |
| | `growth` | Slowly picks up new likes and habits over time; bigger changes need your approval, and the character sheet is never altered | on |
| [The character's life](life.md) | `scene` | The stage setting and scene memories | on |
| | `weather` | Real weather by city, injected each turn as ambient state | off |
| | `belongings` | What is in the fridge and the wardrobe | on |
| | `people` | The character's social circle: friends, family, closeness and their recent news | on |
| | `agenda` | A plan it makes for itself each day and goes and does; a ledger of commitments and promises | on |
| | `relationship` | The stage you are at, what you call each other, understandings, off-limits subjects and the days worth remembering | on |
| | `unspoken` | What it has not said out loud | on |
| | `bookshelf` | Reads the books you put on the shelf, and knows only what she has read | off |
| | `phone` | Scrolls through the feeds you set up when she is free, and brings up what interests her | off |
| | `body_sense` | Per-part familiarity with touch, plus arousal and fatigue | on |
| | `mood` | A mood moved by interaction and drifting back over time | on |
| | `health` | The occasional everyday ailment, rising and falling over its course | on |
| | `presence` | Where it is right now, what it is wearing, how it is sitting, what it is doing | on |
| | `style_watch` | Measures how often assistant-speak shows up, using regex rules | on |
| [Background work](background.md) | `heartbeat` | Speaks first on an interval, with the character setting its own pace | on |
| | `scheduler` | Scheduled tasks (one-off / recurring / daily) | on |
| [Chat channels](im.md) | `qq_bot` | The official QQ open platform | off |
| | `wechat_bot` | The official WeChat ClawBot plugin (bind by QR code) | off |
| | `feishu_bot` | Feishu (China) | off |
| | `lark_bot` | Lark (international) | off |
| | `telegram_bot` | The Telegram Bot API | off |
| | `discord_bot` | The Discord Bot API | off |
| [Maintenance](self-update.md) | `self_update` | Checks for a new release, then downloads, verifies, replaces and restarts in one click | on |
| [External plugins](external.md) | `bundles` | Install Agent Plugins bundles | on |
| | `ext_*` | One card per installed bundle | on |

To write one yourself, see [Project layout and writing plugins](../architecture.md).
