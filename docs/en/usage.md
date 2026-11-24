# Usage and budget

[← Docs home](README.md)　·　[← Back to the project README](../../README.en.md)　·　[中文](../usage.md)　·　English

How much was spent, where it went, and whether to cap it: three questions answered by the ledger, the price table and the budget settings. All three describe **requests this program sent from this machine**, not a provider invoice. Usage of the same key elsewhere is not here, and every amount is an estimate based on prices you entered yourself.

## The ledger: one line per call

Under `usage/` in the configuration directory, one shard per day (`YYYY-MM-DD.jsonl`), one line per call, append-only. There is one criterion: **did this call receive measured usage**. If it did, a line is written — even when the stream then fails, and even when the request falls through to a backup model afterwards (the failed attempt and the successful one are recorded separately). If it did not (some compatible implementations answer successfully without reporting usage), nothing is written and the call is counted neither in the request totals nor in the budget's spent amount.

- The time, the local date it was written under, the type (turn, compaction or helper call) and who started it (empty means the web front end; a plugin is named after itself; the core's own non-interactive helper calls are recorded as `internal`).
- Provider, model, and the input, output, cache-hit and cache-write tokens.
- **Background turns are recorded too** — heartbeat and scheduled work. That is exactly the spending easiest to miss.

Two things are deliberately absent: the Test connection probe in settings, and embedding calls (that endpoint returns no usage).

The ledger is side data, but the ways it can go wrong differ. **At startup**, if the directory cannot be written: the service still starts, the budget goes degraded and **lets everything through** — blocking on numbers you cannot trust is worse than not blocking — and enforcement resumes after one successful write or one successful rescan. **A failed first scan** (this month's history was not read in full) is a separate case: one successful write is not enough, since the history is still missing; only a successful full-month rescan restores enforcement, and that retry comes no earlier than 30 seconds after the previous scan, triggered by the next budget check or status query — there is no background timer. **At runtime**, when a single append fails: that call still counts toward the spent amount and enforcement continues; the ledger merely lacks the line. Only the running service knows about it — the web and IM status, the usage page's budget panel and `GET /api/usage` say the write failed and the totals may be incomplete — while `wen usage` and `wen status` re-add the shards from disk, cannot see a call that never landed, and may report a lower total without any warning. Delete one day's file and that day no longer counts — it is plain JSONL, meant to be aggregated offline.

`wen eval` moves all of its state into a temporary directory, so an evaluation never pollutes the real ledger.

## Prices: turning tokens into money

Prices live on **each model** (settings → Models → a model), per million tokens, with four entries: input, output, cache read and cache write. Leave the two cache prices empty and they are estimated at the input price; reports and the budget mark that as an estimate. Leaving all four empty means the model is not priced: its usage is counted as **unpriced tokens**, and unpriced never means free.

A price carries a currency (up to 8 characters, case-insensitive). Currencies are never converted: reports list them side by side, and the budget only accumulates the one you chose.

The ledger stores tokens, not money; amounts are computed at query time from the **current** price table:

- Change a price and past months of the same ledger are recomputed at the new one. Save the report first if you need the old numbers.
- Saving model settings rescans the current month's budget immediately, so a corrected price does not have to wait for next month to show where the limit really is.

## The budget: what happens at the limit

Budget settings are stored as `usage.json` in the configuration directory. The form under settings → Usage → Monthly budget writes it:

| Field | Meaning |
|---|---|
| `monthly_tokens` | Monthly token limit; 0 means no limit |
| `monthly_cost` | Monthly cost limit; 0 means no limit; needs a price table to take effect |
| `currency` | The currency the cost limit is counted in (default `CNY`) |
| `warn_percent` | Warn once this percentage of the limit is used (default 80); never blocks |
| `over` | What to do at the limit: `skip_background` (default: pause background work, foreground continues) or `block_all` (pause everything) |

A few behaviours worth knowing up front:

- **The two dimensions are judged separately**; exceeding either the token or the cost limit blocks. The cost limit only applies while a price table is available, and the report says outright when it is not in effect.
- **A month is the calendar month**, assigned by each record's local date as written, not by an absolute interval after timezone conversion.
- **A foreground turn always finishes.** The user is waiting for a complete answer; stopping halfway to report the limit costs more than this one turn. The check happens before the next turn.
- **Background turns re-check before every model request**, so a tool loop stops mid-way once the limit is reached. That stop counts as "this beat has passed": the heartbeat does not retry at once, and a scheduled job is not replayed after the limit is raised.
- **An unreadable ledger never blocks.** While a scan keeps failing, everything is let through; the retry comes no earlier than 30 seconds after the previous scan, on the next budget check or status query, and enforcement resumes only once a full-month rescan succeeds — until then the status marks it unavailable, because a broken budget must not lock people out of their own program. **At startup**, an invalid `usage.json` behaves the same way: this run uses the defaults (no limit). Refreshing the usage page afterwards only clears the "unavailable" marker; **the limit does not take effect** — save once from the page or restart the service before the new limit starts blocking.
- **Corrupting `usage.json` at runtime is a different case.** The report says the settings cannot be read and that the old values are still in force, but the running budget keeps enforcing the limits it loaded last; only a save or a restart replaces them (and if the file is still invalid after a restart, that run falls back to no limit).
- Bad shards and failed writes are different: what can be read is still counted and still enforced. `wen usage` reports unreadable shards itself; a write lost at runtime is reported only by the running service, not by anything recomputed from disk.
- Saves from the settings page take effect immediately. `usage.json` may also be edited by hand, but the running budget will not follow; the page then says the new values are not yet in force until you save or restart. Key names are strict — a typo is an error, not something silently ignored — and a file written by a newer version is never overwritten.
- Reaching the limit does not lift on its own. Switching `block_all` to `skip_background` restores foreground requests only; background work stays paused until you raise or remove the limit that fired, or the next calendar month begins.

## Three places to look

- **Web**: settings → Usage. Pick a month and you get requests, tokens, cache hit rate and estimated cost on top; a bar chart by day, breakdowns by source and by model below; unpriced usage and unreadable shards called out separately. The budget form sits on the same page.
- **CLI**: `wen usage` shows the current month by default; `--days N` switches to the last N days and `--by` changes the grouping (day, source, model, kind, provider, provider with model). It reads the ledger files directly and works while the service is running. `wen status` prints the same month summary and budget line. See [CLI](cli.md#wen-usage-this-months-usage).
- **API**: `GET /api/usage?month=YYYY-MM` and `PUT /api/usage/settings`; see [HTTP API](http-api.md).