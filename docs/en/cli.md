# CLI

[← Back to README](../../README.en.md)　·　[中文](../cli.md)　·　English

## Subcommands

```
wen                          start the server (same as wen serve)
wen serve [-c config] [-p port]  start the server; runs the setup wizard first if no model is usable
wen onboard [-c config]          run the setup wizard again: add a provider and switch to it
wen config [section] [-c config] guided configuration (sections: plugins / models / server)
wen plugins [subcommand] [-c config]  manage external plugin bundles (list / install / uninstall / info)
wen status [-c config]           print the current configuration and runtime state
wen eval <script.json> [-o file] replay a scripted conversation and report on style and consistency
wen update [--check] [-y]    check for and install a new version
wen version                  print the version
wen help [command]           show help
```

## wen onboard: the setup wizard

The first time you run `wen` without a usable model, the wizard comes up on its own. You pick a language, a model provider, an API key and a model. The connection is tested once, and finally you confirm the port. It defaults to 8080, and if that port is taken the next free one is suggested. Once you answer, the service starts. Where the answers are stored, and when a `config.yaml` gets created, is described in [Configuration and models](configuration.md#first-run).

You can go through it again any time with `wen onboard`. It adds another provider and switches to it, leaving existing ones alone. If the service is running, it saves through the service's API and the change takes effect at once. In that case it does not ask for the port. It also skips the port when the config directory already has a `config.yaml`, because the wizard never rewrites an existing config file. Change the port there instead.

The wizard needs a terminal. If the service starts outside one (systemd, containers, `nohup`) and no model is configured yet, the program prints a hint and exits instead of waiting for input.

## wen config: changing plugin settings from a terminal

`wen config` exists for **remote deployments**. Plugin toggles and parameters do not live in config.yaml, and until now the only place to change them was the web UI — which is not always convenient to open on a remote machine. It has two modes, and the one in use is stated at the top of the screen:

| Service state | Mode | Behaviour |
|---|---|---|
| Running | Online | Changes go through the service's API and **take effect immediately**; no restart |
| Not running | Offline | The config file is edited directly, and the changes apply at the next start |

Why going through the API is mandatory while the service is up: the plugin state file is rewritten wholesale by the server from memory, so an outside edit neither takes effect nor survives — the server's next write erases it. The mode is decided by `wen.lock` in the config directory (which records the address actually being listened on), confirmed by really connecting once, so a stale record left behind by a killed process does not mislead it.

In offline mode plugins are **registered but not initialized**: reading their config-field declarations and changing toggles and parameters does not require them to actually run, whereas initializing them as usual would bring up every chat channel's long-lived connection along with the heartbeat and the scheduler — and all you wanted was to change one number.

The first item on the top-level menu is `Language`. It changes **this run only** and is forgotten on exit — `wen config` is something you open once every few months, with a person sitting right there at the terminal, so two extra keypresses beat one more state file. To change the language for good, edit `lang:` in config.yaml, creating the file with just that line if it does not exist (see [Configuration and models](configuration.md#language)). The word "Language" and the language names themselves are never translated: once the interface is in a language you cannot read, that is the one line you can still recognise.

The form is generated from the config fields the plugins declare themselves, the same source the settings page uses, so a new plugin gets both interfaces at once. Plugin actions are here too: select the WeChat channel and a "bind by QR code" action appears, with the code drawn as text right in the terminal (black and white regardless of your color scheme, so it scans under a dark theme too), the raw link printed underneath as a fallback, and an automatic refresh and redraw when it expires — no need to open the web UI on a remote server just to scan a code.

## wen plugins: installing external plugin bundles

External plugins follow the [Agent Plugins 1.0](https://agent-plugins.org/) standard. A bundle is a directory with a `plugin.json` at its root, optionally carrying `skills/` (skill manuals) and `mcp.json` (MCP server declarations). Bundles in the Claude, Codex and Cursor formats are read too — the skills and MCP declarations inside them, at least. The standard was published in August 2026 by OpenAI, Microsoft, AWS, Cursor and Vercel, and ChatGPT, Codex CLI, Cursor, GitHub Copilot and VS Code all read the same bundles. It contains no executable code, which is why it works regardless of the language the host is written in.

```bash
wen plugins list                   # what is installed
wen plugins install ./pdf-tools    # from a directory
wen plugins install ./pdf-tools.zip
wen plugins install ./x --no-connect   # install without connecting to its MCP servers
wen plugins refresh pdf-tools      # reconnect and fetch the tool list again
wen plugins info pdf-tools
wen plugins uninstall pdf-tools
```

Installed bundles live in `bundles/<bundle-name>/` under the configuration directory. Each gets its own card in the settings page under [External plugins](plugins/external.md) and its own on/off switch. You can install from there too: fill in a path on the `bundles` card and press Install — same thing as the command below, and it likewise only accepts paths on the machine running the server. Plugin names are prefixed with `ext_`: a plugin name is globally unique and becomes a directory name, so a bundle called `memory` would otherwise collide with the built-in memory plugin.

### Skills

Every subdirectory under a bundle's `skills/` is one skill manual. They join **the same list** as the skills you wrote yourself and share the same `read_skill` tool. They only appear once the "Skill manuals" plugin is enabled — it is off by default, and the command line says so when you install a bundle that carries skills.

### MCP servers

Servers declared in a bundle's `mcp.json` are started, and the tools they offer join the model's tool table. All three transports work: **stdio** (a local subprocess), **Streamable HTTP**, and the deprecated **HTTP+SSE**. Non-loopback addresses must use HTTPS.

Tool names take the form `<bundle>_<server>_<tool>`: tool names are globally unique within wen, and two bundles each carrying a server called `fs` is perfectly normal.

**The tool list is fetched once at install time and stored**, rather than asked for on every startup. Plugins must hand over their tool list at registration (wen checks for global name collisions), and spawning a subprocess per server at that point would stall every startup for several seconds. When a bundle updates, a dependency is installed later, or a server was down last time, use `wen plugins refresh <name>` or the "Shake hands again" button in the settings page.

**Subprocesses start on demand**: if nothing calls a tool, no process is ever spawned. If a connection drops (the server crashed, the machine slept), the next call reconnects once.

Each bundle has two settings: **Confirm before calling** (off by default; when on, every call asks first, and unattended turns — which have nobody to ask — are treated as a refusal) and **Keep at most, per result** (16 KiB by default; how much comes back is the other side's decision, and all of it lands in the context and counts towards compaction).

Every server receives the two environment variables the standard requires: `PLUGIN_ROOT` points at the bundle directory and `PLUGIN_DATA` at `plugins/<plugin-name>/data/`. The latter survives uninstall and reinstall — an index or cache a server built up should not disappear along with the bundle. In `mcp.json`, `args`, the values in `env`, and `cwd` support the `${PLUGIN_ROOT}` and `${PLUGIN_DATA}` placeholders.

### Boundaries

**Restart the server after installing or uninstalling.** Plugins are registered once at startup, and registration order also determines the order in which prompt fragments are assembled. The command line reminds you when a server is running.

**Installing runs the commands the bundle declares**, on your machine, to fetch the tool list. The commands are printed first so you can see them. Pass `--no-connect` to skip that, then run `wen plugins refresh` later.

Paths pointing outside the archive (`../`) and symlinks are refused; unpacking is capped by total size and file count; every path inside a bundle must resolve within the bundle directory. Validation happens before anything is copied, so installing a broken bundle leaves nothing behind. One malformed server is skipped on its own; the rest of the bundle still works.
