# HTTP API

[← Back to README](../../README.en.md)　·　[中文](../http-api.md)　·　English


| Method | Path | Description |
|---|---|---|
| GET | `/api/sessions` | List sessions |
| POST | `/api/sessions` | Create a session |
| GET | `/api/sessions/{id}` | The session's message history |
| DELETE | `/api/sessions/{id}` | Delete a session |
| POST | `/api/chat` | `{"session_id","message"}` → an SSE stream (`delta` / `thinking` / `tool_start` / `tool_result` / `confirm_request` / `confirm_done` / `compact_*` / `done` / `error`) |
| POST | `/api/sessions/{id}/compact` | Compact that session's history by hand (an SSE stream; the frames are the `compact_*` ones from `/api/chat`) |
| POST | `/api/confirmations/{id}` | `{"approved": bool}` answers one confirmation request (the id comes from the `confirm_request` frame). Returns 409 if it has already timed out or been answered |
| GET | `/api/plugins` | The plugin list and their state (including the `source`, the `config_fields` declarations and the `config` values currently in effect) |
| PUT | `/api/plugins/{name}` | `{"enabled": bool}` toggles a plugin at runtime |
| PUT | `/api/plugins/{name}/config` | `{"config": {...}}` saves a plugin's settings; once validated they take effect immediately and are persisted |
| POST | `/api/plugins/{name}/actions/{key}` | Triggers an action a plugin declares (binding WeChat by QR code, say). Returns immediately; the work runs in the background. The body is optional: `{"config": {...}, "token": "..."}`. `config` holds unsaved values from the config dialog, and `token` echoes back the action's `token` from the listing (which thing the button refers to, such as which proposal is being approved) |
| GET | `/api/plugins/{name}/actions/{key}` | Polls that action's progress: its state, its explanatory text and an optional PNG (delivered from memory, never written to disk) |
| GET | `/api/status` | The model configuration and the plugins' status lines; with a `session_id` query parameter, that session's usage as well |
| GET | `/api/events` | A long-lived SSE stream carrying session notices (the lines background work leaves in a session that the model never sees) |
| GET | `/api/models` | Provider and model configuration (`api_key` is returned masked) |
| PUT | `/api/models` | Saves the whole document; an empty `api_key` in the request means "leave it alone" |
| PUT | `/api/models/current` | `{"provider","model"}` switches the current model, effective immediately |
| POST | `/api/models/test` | Tests a connection with one very small real request |
| GET | `/api/auth/status` | Access-control state (whether a password is set, whether this request is authenticated, whether the server listens beyond localhost); reachable before signing in |
| POST | `/api/auth/login` | `{"password"}` signs in and issues a session cookie |
| POST | `/api/auth/logout` | Signs out |
| PUT | `/api/auth/password` | `{"current","new"}` sets or clears the access password; an empty `new` clears it (allowed only while listening on localhost alone) |
