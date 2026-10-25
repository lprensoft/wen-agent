# Documentation

[← Back to the project README](../../README.en.md)　·　[中文](../README.md)　·　English

## Using it

- [CLI](cli.md) — the `wen serve` / `wen config` / `wen status` / `wen update` subcommands
- [Configuration and models](configuration.md) — every option in config.yaml, and provider/model management on the settings page
- [Plugin overview](plugins/README.md) — what each of the thirty-one built-in plugins does, with its settings
- [Deployment and access control](deployment.md) — the authentication model for remote access, a start/stop script, and systemd
- [Replay evaluation (wen eval)](evaluation.md) — turning "does the character feel better after that prompt change?" into a script you can run again

## Design and development

- [How the context is organized](context.md) — what one request is made of, and why the current time is not in the system message
- [HTTP API](http-api.md) — the endpoints shared by the web UI and any external program
- [Project layout and writing plugins](architecture.md) — a tour of the tree, the `Plugin` interface and its optional companions, and visibility scopes

## Decision records

One-off research and design decisions, kept as they stood at the time. They are not updated alongside the code, and they exist in Chinese only.

- [Plugin installation, ecosystem compatibility, and the choice of language](../decisions/agent-plugins-analysis.md) — why the project stays on Go, and why external plugins follow the Agent Plugins standard (2026-08-25, Chinese only)
