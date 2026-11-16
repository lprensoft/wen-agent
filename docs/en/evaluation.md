# Replay evaluation (wen eval)

[← Back to README](../../README.en.md)　·　[中文](../evaluation.md)　·　English


You have just adjusted a prompt — is the character better now, or worse? `wen eval` turns that question from "chat a bit more and see how it feels" into a script you can rerun and judge:

```bash
wen eval docs/eval/example.json                    # report goes to stdout
wen eval docs/eval/example.json -o report.md        # written to a file
wen eval docs/eval/night.json --json run.json      # structured result, for the next comparison
wen eval docs/eval/night.json --baseline run.json  # compare with the last run; exit 1 on deterministic regressions
```

A script is a single JSON file. `name` names the report, `start` sets where the virtual clock begins (RFC 3339; defaults to now), `seed` preloads state, and `turns` lists the steps in order — `{"say": "what you say"}` is one turn, `{"compact": true}` forces a compaction at that point (to see whether the character survives it), `{"gap_hours": 8}` advances the virtual clock by eight hours, so the next turn really is eight hours later (gaps in the history, mood decay and the day boundary all follow it, while the system clock stays put), and `{"run_hours": 24}` lets the character actually live through a day (see below); optional `judge_points` add extra focus points for the judge. See [`docs/eval/example.json`](eval/example.json), and [`docs/eval/week.json`](eval/week.json) for a multi-day run.

`seed` writes state through the plugins' own tools before the run starts, the same way the model calls them:

```json
"seed": [
  {"tool": "save_memory", "args": {"name": "Afraid of cold", "description": "Feels cold below 26°C", "type": "偏好", "content": "..."}},
  {"tool": "update_relationship", "args": {"stage": "熟悉"}, "scope": "inner", "ago_hours": 72}
]
```

`scope` puts the state into one persona; without it the shared domain is used. The value follows the same rules as a plugin name (lowercase `[a-z0-9_]`, starting with a letter) because it is spliced into a data directory, so a bad value is rejected while the script is parsed. `ago_hours` writes that state as many hours ago as you say — 72 means "remembered three days ago", and decay, index ordering and the "N days ago" notes in snapshots all count from that moment. Going through the real tools keeps the fields identical to the plugin docs and independent of the on-disk format.

`expect` hangs off a `say` step and is a deterministic assertion that does not rely on the judge:

```json
{"say": "The office AC is so low today.", "expect": {
  "must_include": ["AC"], "must_not_include": ["as an AI"],
  "max_hits": {"closing_cliche": 0}, "max_sentences": 3, "must_call": ["recall_memory"]
}}
```

Any unmet expectation fails the gate and exits 1. `max_hits` uses the rule ids from `style_watch`; a typo is reported when the script is parsed. An absent or zero `max_sentences` means no limit; a zero inside `max_hits` means the rule may not match at all.

### Long horizons: letting the character live

`gap_hours` only moves the clock; it does not run background work. To make heartbeats, schedule items coming due, scheduled tasks and health reminders actually happen, use `run_hours`:

```json
{"run_hours": 24}
```

It advances event by event: jump to the nearest due moment, run that piece of work, then continue until the requested span has passed. `{"tick": true}` does not move the clock; it only runs what is already due. Background turns appear in the report (time, origin, characters, sentences, hits, summary) and are given to the judge together with the virtual time span; they are **not part of the gate or the baseline regression rule** — their wording is model-variable, while runtime failures are reported separately.

`max_background_turns` caps the whole script's background turn **attempts** (default 100; 0 means the default). It is not a cap on model requests or cost: a heartbeat, a planning turn and a reflection each count as one, while auxiliary calls such as memory extraction do not. Hitting the cap, settlement failing to converge, or a plugin producing same-instant events forever stops the run and keeps whatever has already run.

The first version drives the timeline with four plugins: heartbeat, agenda, scheduler and health. Phone and weather only stop their own loops in an evaluation and produce no events; message channels and maintenance plugins remain off entirely.

It uses the **same configuration and plugins** as `wen serve` (the same config.yaml, models.json and plugins.state.json; persona, voice samples and natural-expression rules all apply), but sessions and plugin data live in a temporary directory that is deleted afterwards, so real data is never touched; message channels and maintenance plugins do not start, while background plugins run on the manual timeline and are driven only by `run_hours` / `tick`. It can run while the service is up.

The report has three parts: the gate and per-turn deterministic metrics (length, sentences, share of action passages, assistant-speak hits, expectation results), the background timeline and a first-half/second-half comparison (differences only, never a failure), then the model's judgement ("reads as one person / tone is consistent / relationship and address are continuous", 1-5 each). With `--baseline` there is a comparison section: more hits, clearly more sentences, or new expectation failures count as regressions; **scores are shown as a trend and never fail the gate**. Before comparing, the run checks that the baseline asks the same questions — a different turn count or a different `say` line is an error rather than a comparison, so edit the script and regenerate the baseline with `--json`. Old baselines (schema 1) still load; they simply carry no background turns.

Limitations: judging is done by a model and varies between runs — use it for trends, not absolute values; assistant-speak detection is a regex heuristic, so a hit is not proof of a bad reply; the background cap counts attempts, not model requests or cost; `seed` can only place state in the past — express future events through the tools' own time parameters (`schedule_task`'s `at`, a commitment's date, a health condition's onset).
