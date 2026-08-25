# External plugins

[← Plugin overview](README.md)　·　[← Documentation](../README.md)　·　[中文](../../plugins/external.md)　·　English

This group works differently from the rest: `bundles` is the built-in entry point for installing things, and the `ext_`-prefixed cards below it are the bundles you installed — as many cards as bundles.

External plugins follow the [Agent Plugins 1.0](https://agent-plugins.org/) standard, published in August 2026 by OpenAI, Microsoft, AWS, Cursor and Vercel. ChatGPT, Codex CLI, Cursor, GitHub Copilot and VS Code all read the same bundles. A bundle is a directory with a `plugin.json` at its root, optionally carrying `skills/` (skill manuals) and `mcp.json` (MCP server declarations), and **no executable code**. The full command-line usage is in [CLI](../cli.md#wen-plugins-installing-external-plugin-bundles).

## `bundles`: the install entry point

| Setting | Default | What it does |
|---|---|---|
| `source` | empty | The bundle to install: a path to a directory or a `.zip` on this machine |

Two actions: **Install** (install whatever `source` points at, then connect to the MCP servers it declares to fetch their tool lists) and **Shake hands with all** (connect to every installed bundle and fetch its tool list again).

`source` only accepts **paths on the machine running the server** — a file sitting in your browser cannot be sent over. For a remote deployment, copy the bundle to that machine first. Uninstalling is not exposed in the interface; use the command line: `wen plugins uninstall <name>`.

This plugin gives the model nothing: no prompt fragments, no tools.

## `ext_*`: the bundles you installed

Every installed bundle gets its own card, named `ext_` plus the bundle name (a plugin name is globally unique and becomes a directory name, so a bundle called `memory` would otherwise collide with the built-in memory plugin). Turning a card off disables both the skills and the tools that bundle carries.

| Setting | Default | What it does |
|---|---|---|
| `confirm` | off | Confirm before calling. What an external tool can do is up to whoever wrote it, so leave this on while you are unsure |
| `max_result_bytes` | 16384 | Truncate any single result beyond this many bytes |

One action: **Shake hands again**, for this bundle only.

**Restart the server after installing or uninstalling.** Plugins are registered once at startup, and registration order also determines the order in which prompt fragments are assembled.

### Skills

Every subdirectory under a bundle's `skills/` is one skill manual. They join **the same list** as the skills you wrote yourself and share the same `read_skill` tool. They only appear once the [`skills`](memory.md) plugin is enabled — it is off by default.

### MCP tools

Tools offered by the servers declared in `mcp.json` join the model's tool table, named `<bundle>_<server>_<tool>`. Three transports are supported: stdio (a local subprocess), Streamable HTTP, and the deprecated HTTP+SSE.

**The tool list is fetched once at install time and stored**, rather than asked for on every startup — plugins must hand over their tool list at registration (wen checks for global name collisions), and spawning a subprocess per server at that point would stall every startup for several seconds. When a bundle updates, a dependency is installed later, or a server was down last time, use "Shake hands again".

Subprocesses start on demand: if nothing calls a tool, no process is ever spawned.

Every server receives the two environment variables the standard requires: `PLUGIN_ROOT` points at the bundle directory and `PLUGIN_DATA` at `plugins/<plugin-name>/data/`. The latter survives uninstall and reinstall — an index or cache a server built up should not disappear along with the bundle.

### Other bundle formats are read too

The Claude, Codex and Cursor bundle formats predate Agent Plugins, and the community has plenty of them. Their manifests sit in places like `.claude-plugin/plugin.json` and carry no `$schema`, but the component locations are the same, so wen reads them as well — **only the skills and MCP declarations inside them**. Anything specific to those formats (commands, subagents, hooks) has no effect, and wen says so when you install one.

## Safety

Installing a bundle means **its declared commands will run on your machine** later, and the tool-list handshake runs them right away. The commands are printed for you at install time. On the command line, `--no-connect` installs without connecting.

Paths pointing outside an archive (`../`) and symlinks are refused; unpacking is capped by total size and file count; every path inside a bundle must resolve within the bundle directory. Validation happens before anything is copied, so installing a broken bundle leaves nothing behind.

If `mcp.json` carries an `Authorization` or `Cookie` header directly, wen points it out: a bundle is meant to be distributed, and a credential written into one is a published credential. It warns rather than refuses — that is the author's call.
