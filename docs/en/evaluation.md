# Replay evaluation (wen eval)

[← Back to README](../../README.en.md)　·　[中文](../evaluation.md)　·　English


You have just adjusted a prompt — is the character better now, or worse? `wen eval` turns that question from "chat a bit more and see how it feels" into a script you can rerun and judge:

```bash
wen eval docs/eval/example.json                    # report goes to stdout
wen eval docs/eval/example.json -o report.md        # written to a file
wen eval docs/eval/night.json --json run.json      # structured result, for the next comparison
wen eval docs/eval/night.json --baseline run.json  # compare with the last run; exit 1 on deterministic regressions
```

A script is a single JSON file. `name` names the report, `seed` preloads state, and `turns` lists the steps in order — `{"say": "what you say"}` is one turn, `{"compact": true}` forces a compaction at that point (to see whether the character survives it), and `{"gap_hours": 8}` only notes "this much time passed here" in the report (it does not change the system clock, and the model cannot see it); optional `judge_points` add extra focus points for the judge. See [`docs/eval/example.json`](eval/example.json).

`seed` writes state through the plugins' own tools before the run starts, the same way the model calls them:

```json
"seed": [
  {"tool": "save_memory", "args": {"name": "Afraid of cold", "description": "Feels cold below 26°C", "type": "偏好", "content": "..."}},
  {"tool": "update_relationship", "args": {"stage": "熟悉"}, "scope": "inner"}
]
```

`scope` puts the state into one persona; without it the shared domain is used. The value follows the same rules as a plugin name (lowercase `[a-z0-9_]`, starting with a letter) because it is spliced into a data directory, so a bad value is rejected while the script is parsed. Going through the real tools keeps the fields identical to the plugin docs and independent of the on-disk format; the trade-off is that "remembered three days ago" cannot be expressed yet — a virtual clock is still on the roadmap.

`expect` hangs off a `say` step and is a deterministic assertion that does not rely on the judge:

```json
{"say": "The office AC is so low today.", "expect": {
  "must_include": ["AC"], "must_not_include": ["as an AI"],
  "max_hits": {"closing_cliche": 0}, "max_sentences": 3, "must_call": ["recall_memory"]
}}
```

Any unmet expectation fails the gate and exits 1. `max_hits` uses the rule ids from `style_watch`; a typo is reported when the script is parsed. An absent or zero `max_sentences` means no limit; a zero inside `max_hits` means the rule may not match at all.

It uses the **same configuration and plugins** as `wen serve` (the same config.yaml, models.json and plugins.state.json; persona, voice samples and natural-expression rules all apply), but sessions and plugin data live in a temporary directory that is deleted afterwards, so real data is never touched; message channels and background plugins (heartbeat, scheduler, QQ / WeChat / Feishu / Telegram) do not start. It can run while the service is up.

The report has two halves: the gate and per-turn deterministic metrics (length, sentences, share of action passages, assistant-speak hits, expectation results), then the model's judgement ("reads as one person / tone is consistent / relationship and address are continuous", 1-5 each). With `--baseline` there is a comparison section: more hits, clearly more sentences, or new expectation failures count as regressions; **scores are shown as a trend and never fail the gate**. Before comparing, the run checks that the baseline asks the same questions — a different turn count or a different `say` line is an error rather than a comparison, so edit the script and regenerate the baseline with `--json`.

Limitations: judging is done by a model and varies between runs — use it for trends, not absolute values; assistant-speak detection is a regex heuristic, so a hit is not proof of a bad reply; `gap_hours` does not make the model feel time pass, and the virtual clock is not built yet; `seed` goes through tools, so past timestamps cannot be specified.
