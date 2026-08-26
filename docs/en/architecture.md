# Project layout and writing plugins

[← Back to README](../../README.en.md)　·　[中文](../architecture.md)　·　English

## Project layout

```
cmd/wen/                 entry point (registers the built-in plugins, wires the layers together)
internal/config/         config loading (YAML plus ${VAR} substitution from the environment)
internal/llm/            the Provider interface plus the OpenAI-compatible and Anthropic implementations
internal/modelcfg/       model and provider configuration (the models.json overlay, hot switching)
internal/agent/          the agent loop (tool calls / thinking / compaction / context budget)
internal/plugin/         the plugin protocol (Plugin / Tool / Configurable / observers / visibility
                         scopes / dependencies) plus Manager (toggles and aggregation)
internal/plugin/builtin/ the built-in plugins (registration order is prompt-assembly order):
                         readfile / execcmd / webfetch /
                         roleplay / dualpersona / scene / weather / belongings / bodysense / health /
                         mood / people / agenda / relationship / unspoken / presence / stylewatch /
                         memory / sessionsearch / skills /
                         heartbeat / scheduler /
                         qqbot / wechatbot / larkbot (Feishu + Lark) / telegrambot /
                         selfupdate
internal/imbot/          the shared skeleton for chat channels (dispatch / commands / session
                         binding / confirmation proxy / channel routing)
internal/cue/            the "reason to speak" board: producers post events, the heartbeat takes
                         them before it opens its mouth
internal/availability/   the "busy" board: the agenda writes while an activity is under way, the
                         heartbeat and the chat channels read
internal/stylecheck/     assistant-speak rules plus the length / sentence-count / narration-share
                         metrics; pure functions, shared by style_watch and wen eval
internal/mdtext/         markdown segmentation and conversion to plain text, shared by every
                         channel's format fallback
internal/textclip/       budget-aware truncation of plugin-injected text, shared by every plugin
internal/statustext/     the wording of status output, shared by every channel's /status
internal/runlock/        running-instance registration (wen.lock), which wen config uses to decide
                         online vs offline
internal/updater/        self-update: find releases, download and verify, replace the executable,
                         restart (each platform its own way)
internal/session/        JSONL session storage
internal/server/         HTTP API + SSE + the embedded web UI
internal/version/        the single source of the version number (UI, /status, the start-up log
                         and the exe's properties all read it)
tools/                   build-time generators: genicon (favicon), genwinres (Windows version resource)
```

## Writing a plugin

Implement the `Plugin` interface from `internal/plugin` (`Name` / `Description` / `Init` / `SystemPrompt` / `Tools`) and register it in `buildPlugins` in `cmd/wen/main.go`. The conventions: prompts and descriptions are written in Chinese and then read through `prompt.T(lang, "key", chinese)` (see [Configuration and models](configuration.md#language); Chinese never goes into a locale pack — it is both the source language and the fallback), describe the capability only, and carry no identity information; returning an empty string from `SystemPrompt()` injects nothing; a plugin name may contain only lowercase letters, digits and underscores (it is used to build a persistence directory).

The optional interfaces cost nothing if you do not implement them:

- `Configurable` (`ConfigFields()`) — declares config fields, from which the settings page generates a form and which it persists. Field types are `int` / `bool` / `string` / `select` / `text` (multi-line, rendered as a textarea).
- `CompactObserver` (`OnCompact()`) — notified **before** a session's history is replaced by a summary, which is the moment to archive or distil. The note you return is appended to the summary, so it lands only in that session's history. The event is broadcast to every subscriber (`memory` and `session_search` both take it and do their own thing); returning an error is only logged and never blocks compaction. When the history carries visibility tags it is grouped by tag, one event per group.
- `ScopeDecider` (`DecideScope()`) and `TurnPrompter` (`TurnPrompt()`) — see "Visibility scopes" below.
- Operation confirmation: take the confirmation channel with `plugin.ConfirmerFrom(ctx)` and ask once before doing something irreversible. A second return value of false means there is no interactive user right now — **do not read it as consent**; the same goes for an error. Not getting an answer is not permission.
- `Dependent` (`Requires()`) — declares plugins that must be enabled alongside it. Enabling is refused while a dependency is unmet (the toggle is greyed out in the UI), and a plugin cannot be turned off while something that depends on it is still on.
- `Conflicting` (`Conflicts()`) — declares plugins whose capabilities work against each other. It warns but does not block.
- `Stoppable` (`Stop()`) — stops whatever background activity the plugin started. Called in three places: on disable, on re-`Init` with new settings, and on process exit. Do cancellation and a bounded wait only; never wait for a whole conversation turn to finish. A plugin that starts goroutines must implement it, and must make `Init` re-entrant.
- `TurnObserver` (`OnTurnEnd()`) — observes the end of each turn. It is broadcast on the synchronous wind-down path, so implementations must return quickly and do anything slow in their own goroutine.
- `StatusReporter` (`StatusLines()`) — contributes one line of runtime state to the status command. Same contract as `SystemPrompt`: cheap and side-effect free.
- `DayReporter` (`DayFacts(date)`) — adds one objective fact about a given day (the weather plugin answers what the weather was that day). Whoever wants the aggregate reads it through `InitContext.DayFacts` — that is how the daily journal gets its heading. Same cheap, side-effect-free contract; return nil when you have nothing to say rather than making something up.
- `Actionable` (`Actions()` / `StartAction()` / `ActionState()`) — declares a flow that can be triggered from the settings page (binding by QR code, say), whose state carries explanatory text and an optional PNG. `StartAction` must return immediately, with any long flow in the background under its own timeout.
- `Categorized` (`Category()`) — declares a functional group (basic tools / memory and search / roleplay / background work / chat channels / maintenance), which the settings page uses to arrange its sections. It affects presentation only, never registration order.
- `FailureTranslator` (`TranslateFailure()`) — a chance to turn a failed turn into an ordinary-looking reply (`roleplay` uses it to play a blocked response as the character drifting off). Single owner, asked in reverse registration order, attempted only on turns with a human present; the translated text is stored as a normal assistant message and the original error goes into a session notice. Configuration errors (`llm.IsConfigError`) must be let through verbatim.
- `NoticeObserver` (`OnNotice()`) — broadcast after a session notice has been written and delivered to the web UI (chat channels use it to push what background work did to the bound user). The event carries the already-truncated stored text.

Three pieces of shared infrastructure sit between plugins, and the core knows about none of them: `internal/imbot` (the chat-channel skeleton, below); `internal/cue` (the "reason to speak" board — producers post **events** rather than state, idempotent per (Source, Key) and always with an expiry; the heartbeat takes every unexpired reason before it speaks. Memory only: the authority on the detected state is the producer's own persistence); and `internal/availability` (the "busy" board — state rather than events, one record per writer, not cleared by reads and expiring on its own deadline; the agenda writes while an activity is under way, and the heartbeat and the chat channels use it to push the next beat back or to hold an inbound message. Memory only as well; after a restart the agenda rebuilds it from the day's plan).

`InitContext` supplies the runtime environment: `Workdir` (the working directory); `StateDir` (the plugin's own persistence directory, `<config dir>/plugins/<plugin>/`, which may not exist yet); `Sessions` (a narrow read-only query over sessions: the most recently active one, whether a given session still exists); `SessionDir` (the session directory, given only to search and archival plugins that must read session bodies); `Complete` (one question-and-answer round with the current model, without tools and without writing to a session); `RunTurn` / `NewSession` / `Compact` (run a full turn as the plugin, create a session, compact a history); `Status` (a snapshot of the model configuration and session usage); `DayFacts` (ask what there is to say about a given day, aggregating every plugin's answer); `Notice` (leave a line in a session for the human only); and `Lang` (this process's language, which a plugin stores in `Init` and thereafter uses for its prompts, tool descriptions, status lines and log output). Anything but `Workdir` being empty or nil means it is unavailable right now, and the plugin should refuse to start or degrade rather than falling back to writing in the process's current directory. Every call to `Complete` and `RunTurn` costs a real model request, so keep them on low-frequency paths.

`RunTurn` and `Notice` are wrapped by the Manager so that the originating plugin is recorded automatically; a plugin cannot pass itself off as the foreground. `RunTurn` returns `ErrSessionBusy` immediately rather than queueing when the session is busy — background tasks piling up on a lock only produce a barrage the instant it is released. What `Notice` writes is stored and shown in the UI in real time, but never enters the model context and never enters a compaction summary: background work finishes after the turn has wound down, when the event stream is already closed, and its result would otherwise only reach the log.

To write another chat channel, note that everything channel-independent (inbound dispatch and deduplication, the command set, per-user serialization, session binding, the confirmation proxy, channel routing) lives in `internal/imbot`. A plugin only implements `imbot.Sender` (send a piece of text to a user) and normalizes inbound messages into `imbot.Message`.

### Visibility scopes

Messages within one session can be partitioned so that some of them are invisible to some turns. What a tag *means* is entirely up to the plugin; the core does exactly three things — tag messages, filter history by tag, and group compaction by tag. It has never heard of "personas". **An empty tag is always visible to every scope**, so history from before the upgrade keeps working, and "shared" holds without any plugin being involved.

There are two stages. `ScopeDecider.DecideScope` settles the scope first (single owner: the first plugin in registration order to return a non-zero `Scope` wins), and `TurnPrompter.TurnPrompt` then injects one-shot prompts for the scope already decided. They are separate because `memory` has to filter its index by readable scope while another plugin decides the scope, and a single broadcast stage gives no ordering between the two. The scope then travels to tool execution through the `context`; take it with `plugin.ScopeFrom(ctx)`.

The signature of `SystemPrompt()` does not change: its contract is to be cheap, side-effect free and callable at any time (the plugin-list endpoint calls it even for **disabled** plugins), so anything that needs session context goes through `TurnPrompt`.

To keep data separated by scope, use `plugin.DomainDir` and `plugin.ReadDomains`: the empty tag uses the base directory itself, and every other scope uses a sibling `<base>-<tag>`. Because a tag ends up in a path, it is validated against the same character set as a plugin name, and a decision that fails validation is discarded entirely.

Scopes can also be bound to channels: once a plugin installs `imbot.SetRouter`, each channel delivers according to "which channel should this session's replies go to", regardless of which channel this turn came in on (`dual_persona` uses it to give the outer and inner personas one IM channel each). That part lives in `internal/imbot`; the core takes no part in it, and `imbot` itself only knows channel names.

**A visibility scope is context isolation, not a sandbox** — any plugin offering a general file or command channel can go around it.
