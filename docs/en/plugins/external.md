# External plugins

[← Plugin overview](README.md)　·　[← Documentation](../README.md)　·　[中文](../../plugins/external.md)　·　English

This group works differently from the rest: `bundles` is the built-in entry point for installing things, and the `ext_`-prefixed cards below it are the bundles you installed — as many cards as bundles.

External plugins follow the [Agent Plugins 1.0](https://agent-plugins.org/) standard, published in August 2026 by OpenAI, Microsoft, AWS, Cursor and Vercel. ChatGPT, Codex CLI, Cursor, GitHub Copilot and VS Code all read the same bundles. A bundle is a directory with a `plugin.json` at its root, optionally carrying `skills/` (skill manuals) and `mcp.json` (MCP server declarations), and **no executable code**. The full command-line usage is in [CLI](../cli.md#wen-plugins-installing-external-plugin-bundles).

## `bundles`: the install entry point

| Setting | Default | What it does |
|---|---|---|
| `source` | empty | The bundle to install: a path to a directory or a `.zip` on this machine |
| `update_name` | empty | The installed bundle name whose update source is being bound or removed |
| `update_url` | empty | The publisher's HTTPS `update.json` URL |
| `update_key` | empty | The publisher's Base64-encoded Ed25519 public key |
| `auto_check` | on | Check automatically, verifying manifests only; never install automatically |
| `check_hours` | 24 | Check interval, 1–720 hours |

Four actions: **Install**, **Shake hands with all**, **Bind update source** and **Unbind**. Installation connects to the bundle's MCP servers to fetch their tools. Unbind before changing the update URL or key, and cancel any pending update first.

`source` only accepts **paths on the machine running the server** — a file sitting in your browser cannot be sent over. For a remote deployment, copy the bundle to that machine first. Uninstalling is not exposed in the interface; use the command line: `wen plugins uninstall <name>`.

This plugin gives the model nothing: no prompt fragments, no tools.

## `ext_*`: the bundles you installed

Every installed bundle gets its own card, named `ext_` plus the bundle name (a plugin name is globally unique and becomes a directory name, so a bundle called `memory` would otherwise collide with the built-in memory plugin). Turning a card off disables both the skills and the tools that bundle carries.

| Setting | Default | What it does |
|---|---|---|
| `confirm` | off | Confirm before calling. What an external tool can do is up to whoever wrote it, so leave this on while you are unsure |
| `max_result_bytes` | 16384 | Truncate any single result beyond this many bytes |

Actions depend on the bundle and update state: **Shake hands again** reconnects this bundle only; a bound source adds **Check for updates** and **Update**, and a staged update adds **Cancel pending update**. Skill-only bundles can be checked and updated too.

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

## Signed updates

Wen does not infer an update source from `repository` or `homepage`. Install one version manually, then bind its name, update manifest URL and publisher public key on the `bundles` card. The CLI covers the same flow:

```bash
wen plugins track pdf-tools https://example.com/pdf-tools/update.json <Base64-public-key>
wen plugins check pdf-tools          # omit the name to check every source
wen plugins update pdf-tools         # confirm manually and stage the verified candidate
wen plugins update pdf-tools --cancel
wen plugins untrack pdf-tools
```

`info` shows the source, key fingerprint, last check, candidate and pending restart state. While the service is running, these state-writing CLI operations are refused; use the settings page so two processes cannot write update state concurrently.

### What a publisher provides

Any static HTTPS hosting works; a GitHub Release can host the ZIP. Each bundle needs one manifest and one detached signature:

```text
https://example.com/pdf-tools/update.json
https://example.com/pdf-tools/update.json.sig
```

```json
{
  "schema_version": 1,
  "name": "pdf-tools",
  "version": "1.3.0",
  "archive_url": "https://example.com/releases/pdf-tools-1.3.0.zip",
  "sha256": "<64 lowercase hexadecimal SHA256 characters for the ZIP>"
}
```

- Signatures use Ed25519. `.sig` is the Base64 encoding of a 64-byte signature over the **raw response bytes of `update.json`**, not parsed and re-serialized JSON. The Base64-encoded 32-byte public key is given to users through a separate trusted channel.
- The ZIP may live on another domain because its URL and digest are signed. Manifests, signatures and downloads require HTTPS, redirects may not downgrade to HTTP, and URLs carry neither user information nor fragments.
- The manifest name, the name in the ZIP's `plugin.json` and the installed name must match, and the ZIP version must equal the signed version. ZIP download and extraction each have a 64 MiB limit, with at most 5000 extracted files.
- Versions must be comparable as `vX.Y.Z` (the `v` is optional) or the existing prerelease forms. Missing or unrecognised versions do not participate; same-version and downgrade candidates are not updates, and there is no forced downgrade.

### Check, stage and restart boundaries

By default, the background check downloads only the manifest and signature once a day, verifies them with the bound key and records a candidate. **It never downloads the ZIP, replaces a bundle, reconnects or restarts.** It waits two minutes after startup and at least one hour after a failure; one failing source does not block the others.

Only **Update** or `wen plugins update` downloads the exact candidate ZIP and rechecks its signature, digest, name, version and directory safety before staging it in full. The action token identifies the candidate shown on screen; if another window changes it, the old action is refused. Staging leaves the live bundle and tool cache untouched. The swap happens on the next actual service start, after checking that the live bundle did not change since staging; failures restore the old bundle.

Updates preserve `PLUGIN_DATA`, plugin switches and tool settings, and **never run MCP commands from the new bundle automatically**. A skill-only bundle needs one restart. After an MCP bundle update, reconnect manually and restart once more to register the new tool list. A staged update can be cancelled before that restart.

A bound key authenticates **future updates only**; it does not prove that a previously hand-installed bundle came from that publisher. SHA256 proves that the ZIP matches the signed manifest, while publisher identity comes from your pinned public key. Unbind before changing a URL or key; keys never rotate online automatically.

Backups include update sources and public keys (URLs may reveal private deployment details), but not pending, staging or rollback directories, so a restore cannot bring back a half-finished update.

## Safety

Installing a bundle means **its declared commands will run on your machine** later, and the tool-list handshake runs them right away. The commands are printed for you at install time. On the command line, `--no-connect` installs without connecting.

Paths pointing outside an archive (`../`) and symlinks are refused; unpacking is capped by total size and file count; every path inside a bundle must resolve within the bundle directory. Validation happens before anything is copied, so installing a broken bundle leaves nothing behind.

If `mcp.json` carries an `Authorization` or `Cookie` header directly, wen points it out: a bundle is meant to be distributed, and a credential written into one is a published credential. It warns rather than refuses — that is the author's call.
