# Basic tools

[← Plugin overview](README.md)　·　[← Back to README](../../../README.en.md)　·　[中文](../../plugins/tools.md)　·　English

## Basic tools (read_file / web_fetch plugins)

Two plugins with little to say about them, listed here only for completeness. `web_fetch` is on by default; `read_file` is off — it can read any file, session files and the memory directory included (visibility scopes are context isolation, not a sandbox), so turn it on from the settings page when you want it.

- **`read_file`** — reads a local text file, truncating past the limit. The tool has the same name.
- **`web_fetch`** — fetches a web page and returns its text (`fetch_url`), with limits on both the timeout and the size.

| Plugin | Setting | Default | Description |
|---|---|---|---|
| `read_file` | `max_bytes` | 65536 | The most bytes one read returns |
| `web_fetch` | `timeout_seconds` | 20 | The longest to wait for a single page |
| `web_fetch` | `max_bytes` | 65536 | The most bytes extracted from a single page |

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
