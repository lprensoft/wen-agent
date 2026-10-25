# Chat channels

[← Plugin overview](README.md)　·　[← Back to README](../../../README.en.md)　·　[中文](../../plugins/im.md)　·　English

## Using it remotely (qq_bot / wechat_bot / feishu_bot / lark_bot / telegram_bot plugins)

Five channels, each mapping a remote user to an ordinary session — a remote session *is* an ordinary session, visible in the web UI on a refresh, with both sides talking to the same history. All off by default.

Everything channel-independent (inbound dispatch and deduplication, the command set, per-user serialization, session binding, the confirmation proxy, pushing) has a single implementation, and each plugin is responsible only for its own protocol layer, so the conventions below are identical across all five channels:

- **Commands**: `/new` starts a new session, `/status` shows the model and usage, `/compact` compacts by hand, `/help`, plus `/apply` and `/deny` for confirming dangerous operations.
- **Anyone outside the allowlist is refused**, with only a log line. An empty allowlist refuses everyone; a stranger's id is written to the log so you can copy it into the list.
- **Dangerous operations can be confirmed remotely**: when `exec_command` needs confirmation, the request is pushed to the remote side and waits for `/apply` or `/deny`; a timeout counts as a refusal. Not getting an answer is not permission.
- **Background turns are pushed**: when a background turn such as a heartbeat or a scheduled task lands on the bound session, the assistant's final text is pushed to the remote side — otherwise the result only reaches the session file and you would never see it on your phone. Foreground turns, turns the channel itself started, and **turns started by another channel** are not pushed; each of those has its own way of replying.
- **Optional process forwarding**: "show the thinking" and "show tool calls" are both off by default. Only tool names are forwarded; arguments and results may carry private information.
- **Optional background notices**: "push background notices" (off by default) pushes the notes background work leaves in a session — a memory distillation's record, say — to the bound user, word for word as the web UI shows them, without the push itself writing anything to the session. Only notices produced by plugin background work are pushed. Four kinds are not: ones carrying a visibility tag (ownership is uncertain, and it is better to push less than to leak one persona's business to the other), notes from a foreground turn (the original error text left behind by a failure translation, for instance — what the front door hides for the sake of immersion does not leak out the side), notes the channel itself wrote for the operator, and anything not routed to this channel.
- **Merging messages sent in quick succession**: people often split one thought across three messages. "Merge rapid messages" (3 seconds by default) merges ordinary messages sent by the same person within the window into a single turn before replying; each new message restarts the clock, and the total wait never exceeds three windows. Messages that arrive while a reply is in progress are merged into one turn too when they come off the queue. Commands (`/new` and friends) do not take part and are handled immediately, with whatever has accumulated in the window becoming a turn first. Set it to 0 to reply to each message separately.
- **Texting like a person**: "text like a person" (off by default) does three things: splits a reply into several messages by paragraph and sentence (about 120 characters each, at most 6, with code blocks and tables never split), pauses between them to simulate typing based on the length of the previous one (0.4–2.5 seconds, showing "typing" meanwhile), and asks the model to talk the way people text on turns started by this channel (short sentences, spoken register, no headings, lists or bold). Text pushed from a background turn is split as well. Command receipts, error messages, confirmation requests and process notes are neither split nor delayed, and the web UI and CLI are unaffected. With it off, the behaviour is exactly what it was before.
- **Replying later when busy**: "reply later when busy" (off by default) stops the character from answering instantly while busy or asleep. Busy means an activity in progress on the [agenda](life.md) or a sleep from the [heartbeat](background.md). When the character is very busy, a message waits at most ten minutes (a quick glance at the phone). When the character cannot reply at all, it waits until the activity ends, or less if it ends early. When asleep, messages wake the character after "minutes until woken" (15 by default). Lightly busy means replying as usual. Nothing shows "typing" while a message waits, and several messages sent in the meantime are answered as one turn. That turn tells the character how long ago the message was sent and what was going on at the time; whether to apologise and how to explain is the character's call. The wait never exceeds the platform's passive-reply window (45 minutes at most on QQ), and commands are still handled at once. Held messages are saved to disk, so after a restart the wait continues from their original arrival time. The web UI is unaffected.
- **Routing by persona**: with `dual_persona`'s split channels on, a reply goes to the channel of the persona currently in charge, regardless of which channel you spoke on (see [dual persona](roleplay.md)).

Their protocol layers:

| | Channel | Credentials | Worth knowing |
|---|---|---|---|
| `qq_bot` | The official QQ open platform, WebSocket event gateway | AppID / AppSecret | A passive reply carries a msg_id (at most 4 replies to one message within 60 minutes, after which it falls back to an active message); C2C direct messages |
| `wechat_bot` | The official WeChat ClawBot plugin (iLink), HTTP long polling | **Bind by QR code** from the gear on the settings page — the credential is not a key you can paste, but something the server issues after you confirm by scanning | A reply must carry the inbound message's context_token, so the most recent ticket is persisted per user; nothing can be pushed to someone who has never written first |
| `feishu_bot` | Feishu (China), events over the official SDK's long-lived connection, no public address needed | App ID / App Secret | Grant the `im:message.p2p_msg:readonly` (read direct messages) and `im:message:send_as_bot` (send as the app) scopes in the developer console, choose "long connection" for event subscription and subscribe to the receive-message event; direct messages only |
| `lark_bot` | Lark (international), same as above | Same as above, but **not interchangeable with Feishu** | The same implementation instantiated twice. They are two plugins because an international app cannot be used with the China edition, the credentials and open_ids are separate, and one plugin can hold only one set of credentials and so could only connect to one side |
| `telegram_bot` | The Bot API, HTTP long polling | Bot Token | Not reachable directly from mainland China, so configure `proxy` (http / socks5). A 409 at start-up means the bot has a webhook set, which you need to `deleteWebhook` yourself first |

| Setting | Default | Description |
|---|---|---|
| `app_id` / `app_secret` | empty | `qq_bot` and Feishu / Lark. The secret lives in the plugin state file (0600) and is not committed |
| `bot_token` | empty | `telegram_bot` only |
| `proxy` | empty | `telegram_bot` only, shaped like `http://…` or `socks5://…`; empty connects directly |
| `whitelist` | empty | Allowed user ids, one per line (openid for QQ, `xxx@im.wechat` for WeChat, open_id for Feishu / Lark, chat_id for Telegram) |
| `api_base` | the official address | Rarely needs changing |
| `confirm_timeout_sec` | 300 | The longest to wait for `/apply` / `/deny`; a timeout counts as a refusal |
| `format` | see right | QQ / WeChat / Telegram offer `markdown` (falling back to plain text when the platform refuses) and plain text; Feishu / Lark offer cards (the markdown component of card JSON 2.0, which renders headings, lists, quotes and code blocks) and plain text |
| `show_thinking` / `show_tools` | off / off | Whether to forward the reasoning and the tool names to the remote side |
| `push_notices` | off | Whether to push what background work recorded (a memory distillation's note, say) to the remote side |
| `merge_window_sec` | 3 | The window for merging rapid messages, in seconds; 0 turns it off |
| `human_pace` | off | Text like a person: split replies, typing pauses, and a nudge to the model to use a conversational register |
| `busy_hold` | off | Reply later when busy: while the character is busy on the agenda or asleep, messages are answered a while later |
| `wake_after_minutes` | 15 | With reply later when busy on, how long messages take to wake a sleeping character; 0 = the first message wakes them |
