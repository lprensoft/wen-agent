# Basic tools

[← Plugin overview](README.md)　·　[← Back to README](../../../README.en.md)　·　[中文](../../plugins/tools.md)　·　English

## Basic tools (read_file / web_fetch / web_search / media plugins)

These plugins are listed here only for completeness. `web_fetch` and `media` are on by default; `read_file` is off — it can read any file, session files and the memory directory included (visibility scopes are context isolation, not a sandbox), so turn it on from the settings page when you want it; `web_search` is also off — search terms go to Tavily, and the key is optional, with a free quota when left empty.

- **`read_file`** — reads a local text file, truncating past the limit. The tool has the same name.
- **`web_fetch`** — fetches a web page and returns its text (`fetch_url`), with limits on both the timeout and the size. Public addresses only; see below.
- **`web_search`** — searches the web and returns a few page snippets (`search_web`), with configurable result count, hourly cap and timeout; the key is optional, see below.
- **`media`** — sends an image or file from the working directory to whoever this conversation belongs to (`send_image` / `send_file`), on message channels and background turns alike; see below for the limits and the path confinement.

| Plugin | Setting | Default | Description |
|---|---|---|---|
| `read_file` | `max_bytes` | 65536 | The most bytes one read returns |
| `web_fetch` | `timeout_seconds` | 20 | The longest to wait for a single page |
| `web_fetch` | `max_bytes` | 65536 | The most bytes extracted from a single page |
| `web_search` | `api_key` | empty | Tavily API key; leaving it empty uses the keyless free mode with a tighter quota |
| `web_search` | `results` | 5 | How many snippets one search returns (1–10) |
| `web_search` | `max_per_hour` | 20 | Searches per hour, at most (1–200); failed requests do not count |
| `web_search` | `timeout_seconds` | 20 | The longest to wait for one search (1–120 seconds) |

`fetch_url` reaches public addresses only: loopback, private networks, cloud metadata endpoints and any redirect between them are refused. The check sits at the dialling layer, so a 302 cannot get around it, and the address that is checked is the address that is connected to (a page teaching the model to fetch `169.254.169.254` is the usual shape of this risk).

When an HTTP proxy is configured, the hop to the proxy itself is not checked: local proxies are common, and refusing them would leave those users unable to fetch anything at all; the target host is resolved by the proxy, which this layer cannot see. If you really need to read something on a network this machine can reach, use `exec_command`, the path that already asks a human first.

## Searching the web (web_search plugin)

When enabled it provides `search_web`, which hands the query to Tavily and returns a few page snippets. Leaving `api_key` empty sends a keyless request with a tighter quota; filling it in buys quota, not availability. Search terms go to a third party, which is why the plugin is off by default. The hourly cap counts only successful requests; calls above it are refused outright so an enthusiastic conversation cannot burn through the quota.

## Sending media (media plugin)

`media` is on by default and provides two tools:

- `send_image`: sends a PNG / JPEG / GIF from the working directory (5 MB per image) to whoever this conversation belongs to, and keeps it in the conversation — the same image shows up in the assistant's bubble in the web UI.
- `send_file`: sends an ordinary file from the working directory (20 MB per file). The content is not parsed; the MIME type comes from the extension and a content sniff.

The outlet follows this turn's text: an IM turn started by a real person uses its own reply credentials and the split-channel router; background turns such as heartbeats and scheduled tasks have no inbound message and push to the users bound to the session; a web UI turn has no external exit, so the image stays in the session and the web UI. When the router sends a reply to another channel, the media goes there too, and if that channel cannot deliver it the call fails rather than falling back to the originating channel. When a channel is available but nobody is bound to this session, the image still goes into the conversation and the tool says plainly that there was no channel to deliver it to. Paths are confined to the working directory, and symlinks are resolved before the check so a link cannot reach files outside it. When the platform says "possibly delivered but unconfirmed", the tool says so explicitly instead of treating it as an ordinary failure, so the model does not send it twice. See [chat channels](im.md) for each channel's upload flow and limits; QQ's official rich-media API still marks its file type as not open, so `send_file` reports that clearly.

## How command execution is kept safe (exec_command plugin)

This tool hands an arbitrary string to `cmd /C` or `sh -c`, which makes it the one place in the system capable of irreversible damage. **It is off by default** — you want it when you are using this as an agent that does work for you, which is not what it should be out of the box; turn it on from the settings page, where it is a visible choice. Once on, the approach taken is **intercept before executing and ask a human**, not a sandbox.

**Why not a sandbox.** A shell has `&&`, `|`, variable expansion, `for /f`, base64 decoding, UNC paths, `..\..` — working out "which files will this touch" by analysing the command text simply cannot be done. A path restriction that can be trivially bypassed is worse than none, because people trust it. Real isolation has to come from the operating system (containers / seccomp / AppContainer), and that conflicts with the whole point of this tool — running builds, tests and git in a real working directory. You would have to put the toolchain, the network and your git credentials inside, and once you have, nothing is isolated any more. What actually prevents an accidental deletion is **a human looking at the `rm -rf` before it runs**, and only a human can judge intent.

Three verdicts:

| Verdict | What happens | Examples |
|---|---|---|
| Refused | Neither asked nor executed; the reason goes back to the model | Formatting a whole disk, writing to a raw device, `rm -rf /`, deleting the system root, destroying shadow copies, changing `bcdedit`, shutting down or rebooting, fork bombs |
| Confirmation required | The turn blocks, a card appears in the UI, and it waits for you to click "allow" or "refuse" | Deleting files, mirror sync (which deletes extra files on the target), `git reset --hard` / `--force` / `rebase`, changing permissions, elevating privileges, killing processes, changing services and the registry, installing and removing packages, piping into a shell, dropping containers and tables |
| Allowed | Executed directly | Everything else (at the `dangerous` level); read-only commands (at the `all` level) |

The most dangerous tier deliberately does not ask: putting "format the disk?" in front of a person is itself an opportunity to misclick.

A few implementation details:

- **Chained commands are judged segment by segment and as a whole.** Segment by segment so nothing hiding in the second half is missed (`go build && rm -rf out`); as a whole so patterns that span a `|` are not missed either (`curl … | sh`, fork bombs). When several categories match, all of them are listed — one command may delete files *and* rewrite commit history, and mentioning only one would leave you thinking you had seen what you were agreeing to.
- **Not getting an answer is not permission.** A timeout, a closed page, a dropped connection — all count as a refusal. There is deliberately no "allow it when nobody is around" switch either: if you really do need destructive commands to run unattended, turn `guard` off. That is a visible choice rather than an exception that is easy to forget.
- **The model is told about this rule**, because a model that is refused without knowing why will rewrite the command and try again, which is exactly the behaviour to avoid. The prompt also asks it to run destructive operations on their own rather than chaining them, so that you can approve only the safe part.

**It guards against mistakes, not attacks.** The `dangerous` level judges by command text, and deliberate obfuscation (base64, string concatenation, writing a script and running it) gets around it. The only defence against that is the `all` level — everything outside the read-only allowlist gets confirmed.

| Setting | Default | Description |
|---|---|---|
| `guard` | `dangerous` | Interception level: `dangerous` / `all` / `off` |
| `timeout_seconds` | 60 | The longest a single command may run |
| `confirm_timeout_seconds` | 300 | The longest to wait for confirmation; a timeout counts as a refusal |
