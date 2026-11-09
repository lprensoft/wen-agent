# P1 外部协议核对：Responses / 视觉 / 媒体 / 语音 / 向量化

[← 返回文档索引](../README.md)　·　调研记录，只有中文

> **核对日期 2026-11-09。** 对象是 P1 计划里依赖外部协议的四块：OpenAI Responses API、视觉输入、各 IM 通道的媒体收发、语音（TTS/ASR）与向量化。记录的是「协议现在长什么样、我们的接口要改哪里」，不是路线图承诺；协议会变，据此动手前先核对原文。
>
> **证据等级：**
>
> - **【原文】** 直接读到官方文档正文（OpenAI / Anthropic / 飞书 / QQ / 钉钉 / 企业微信 / Telegram / Discord / Slack）。
> - **【SDK】** 读官方 SDK（`openai-python`、`anthropic-sdk-go`）的类型定义，等价于官方 OpenAPI 的线上形状。
> - **【推断】** 由代码或文档边界推出来、但官方没有直接写明的结论。
> - **【待验】** 没有凭证或文档站不可达，必须真机 / 控制台复核。
>
> OpenAI 文档站对普通 HTTP 抓取返回 403，本次是用浏览器读渲染后的正文；IM 文档站多为前端渲染，同样用浏览器读。没有调用任何真实 API——所有端点级行为都还属于【待验】。

## 结论摘要

1. **Responses API 能接，但不能复用现在的 `llm.Message`。** 它的输入/输出是 Item 数组，不是 role/content 消息；函数调用、推理项、工具输出各自是独立 Item。要接就得在会话里保存一层「原始 Item」，并决定状态放在哪。**建议 `store: false` + 完整回放 Item**：不把会话交给 OpenAI 保管，代价是历史里要保留加密推理项。
2. **视觉输入的差异只在 wire 层，能力边界很硬。** OpenAI 单请求 512 MB / 1500 张，Anthropic 单图 10 MB、单请求 128 MB；两家都不支持 HEIC，GIF 只取第一帧。IM 下载下来的原图经常超过这些限制，必须先按字节数降采样，不能先 base64 再试。
3. **九条 IM 通道里八条有完整的媒体收发文档，微信 iLink 没有公开文档。** 图片都支持；文件、语音、视频各有格式和大小限制，且企业微信的下载链接带 AES 密钥、5 分钟过期。P1-3 应按「文档完整度」分批做，微信放最后。
4. **语音这块最大的变化是 OpenAI 的 `/v1/audio/speech` 已进入退役倒计时（2027-01-06），转录侧旧模型 2027-02-26 退役。** 现在不该把 TTS 建在 Speech API 上；ASR 用 `gpt-transcribe` 的文件接口仍可行，实时转录走 Realtime。Anthropic 两边都没有。
5. **向量化不能挂在聊天提供商上。** OpenAI 有 `/v1/embeddings`（8192 token 输入、可调维度），Anthropic 明确没有嵌入模型、官方推荐 Voyage。`models.json` 需要独立的 embedding 选择，记忆插件的召回要从关键词打分升级成「可选向量重排」。

## 一、OpenAI Responses API

### 1.1 线上形状【原文】

- 端点：`POST /v1/responses`，与 `/v1/chat/completions` 并列；Chat Completions 仍受支持，但官方把所有新项目指向 Responses。
- 输入：`input` 可以是字符串，也可以是消息数组；系统级指令放 `instructions`，不再必须塞一条 `system` 消息。
- 输出：`output` 是 Item 数组，常见 `message`、`function_call`、`reasoning`；文本快捷属性叫 `output_text`。没有 `choices`，也没有 `n`。
- 工具：`tools: [{"type":"function","name":...,"description":...,"parameters":...,"strict":...}]`；模型返回 `function_call` Item，应用把 `{"type":"function_call_output","call_id":...,"output":...}` 追加进下一次 `input`。
- 流式：SSE 事件带 `type` 与 `sequence_number`。已核对到的事件名包括 `response.output_text.delta`、`response.output_item.added`、`response.output_item.done`、`response.function_call_arguments.delta`、`response.reasoning_summary_text.delta`、`response.completed`、`response.incomplete`、`response.failed`、`error`。
- 结构化输出：`text.format`，不是 Chat 的 `response_format`。
- 生成上限：`max_output_tokens`，不是 `max_tokens` / `max_completion_tokens`。
- 推理：`reasoning.effort` 控制强度；`reasoning.context` 取 `current_turn` / `all_turns`，控制前几轮的推理项要不要重新渲染进上下文。

### 1.2 状态：本项目最关键的一条【原文】

- 响应默认会被存储；`store: false` 进入无状态模式，组织启用 ZDR 时同样如此。
- 无状态模式下，`reasoning` Item 默认带 `encrypted_content`。官方明确给了做法：**保留每次响应的全部 output Item，加上下一条用户消息，整段回放**；要在后续轮次继续利用推理，必须把这些 Item 原样带回去。
- 官方同时说明：函数调用场景下，返回的推理 Item 必须和工具输出一起回传，否则模型无法接着推理。
- `previous_response_id` 可以把上一轮串起来，省掉回放；但那是把会话状态放到 OpenAI 侧。

**对本项目的判断：**

- 会话正文、压缩摘要、可见域、跨提供商切换的权威都在本地。用 `previous_response_id` 会把「历史是什么」分叉成两份，压缩和换模型都会失效，**不采用**。
- 采用 `store: false` + 回放。需要给 `llm.Message` 增加一层不透明字段（类似现在的 `ReasoningBlocks`），保存 `reasoning` Item 的 `encrypted_content` 与 `function_call` Item 的原始形状。压缩时保留消息正文与工具调用结果；旧推理项能否丢取决于 `reasoning.context` 的取值，动手前需用 `current_turn` 实测确认，不能先假定可以省。
- 回放顺序必须是「原输入 → 本轮全部 output Item → 下一条用户消息」，不能按 role 重新拼；把 Item 压平成消息会丢掉 `call_id` 与推理关联。

**兼容性风险：**

- `/v1/responses` 目前不是所有「OpenAI 兼容」提供商都实现。接入前要逐家确认，不能因为 `base_url` 是 OpenAI 兼容就默认可用。**建议把它做成每个提供商/模型上的显式协议选项（`chat` / `responses`），默认仍是 `chat`。**
- 现有备用链只按 `provider + model` 切换；如果主模型走 Responses、备用模型只支持 Chat，切换时要换的不只是模型名，还有整轮请求的 wire 形状。`ModelTarget` 需要带上协议字段，回退时按目标协议重建请求。

### 1.3 流式映射建议

| Responses 事件 | 现有 `StreamEvent` |
| --- | --- |
| `response.output_text.delta` | `EventContentDelta` |
| `response.reasoning_summary_text.delta` / `response.reasoning_text.delta` | `EventReasoningDelta` |
| `response.function_call_arguments.delta` | 累积到该 `item_id` 的参数缓冲 |
| `response.output_item.done`（`function_call`） | 先按 Item 累积；全部工具调用到齐后一次性发 `EventToolCalls` |
| `response.completed` | `EventUsage` + `EventDone` |
| `response.incomplete` / `response.failed` / `error` | `EventError`，保留 `code` 供瞬态判定 |

瞬态判定可以复用 `llm.IsTransientError`：错误事件里的 `code` 要与现有 `APIError.Code` 对齐；内容拦截、鉴权、参数错误仍不触发备用切换。

**工具调用不能逐项发 `EventToolCalls`。** `internal/agent/agent.go` 对它是整体赋值（`r.toolCalls = ev.ToolCalls`），不是追加；一次响应里有多个 `function_call` 时，逐项发送只会留下最后一个。适配层必须等全部 `function_call` Item 到齐后统一发一次，或每次发送当前累计的完整列表。

## 二、视觉输入

### 2.1 OpenAI【原文】

- 格式：PNG、JPEG、WEBP、非动画 GIF。
- 单请求：最多 512 MB 总负载、1500 张图；按 patch 计最多 30000 patch/图，超限直接拒绝而不是自动再缩。
- `detail`：`low` / `high` / `original` / `auto`，默认 `auto`。
- Responses 的图片 part 是 `{"type":"input_image","image_url":"<URL 或 data URL>","detail":...}`，也可用 `file_id`。
- Chat Completions 的形状不同：`{"type":"image_url","image_url":{"url":"<URL 或 data URL>","detail":...}}`【SDK】。

### 2.2 Anthropic【原文】

- 三种 source：base64、URL、Files API 的 `file_id`；Bedrock / Google Cloud 只支持 base64。
- 格式：JPEG、PNG、GIF、WebP；动画不支持，只取第一帧。
- 单图上限：Claude API 10 MB（base64 后）、Bedrock 5 MB；单请求 128 MB；图片数上限 100（200k 上下文）或 600（其他模型）；单边最大 8000 px。超过 20 张时还有更严格的单图尺寸限制，官方建议每边不超过 3000 px。
- 计费按 28×28 patch 计视觉 token。

### 2.3 对本项目的影响

- `llm.Message.Content` 现在是 `string`，要扩成「文本 + 图片 part」；Anthropic 还要多一层 source 判别，OpenAI 两种协议的形状也不同。**不要把图片塞进 data URL 字符串再当文本传**，否则压缩、可见域、会话检索都会把 base64 当正文。
- IM 侧必须做下载上限与降采样：Telegram 20 MB、飞书 100 MB、企业微信图片 10 MB、QQ 软限 20 MB——都大于或接近模型单图限制。建议统一在写入会话前缩到长边 ≤ 3000 px；单图预算要按 **base64 编码后** 的长度校验：Bedrock 的 5 MB 上限对应原始字节约 3.75 MB，按原始 5 MB 卡会超限；Anthropic 单请求 128 MB 的总量也要单独检查。
- HEIC 两家都不支持。iOS 相册直传可能是 HEIC，需要转码或明确提示；Go 标准库不能解 HEIC，这是 P1-3 的一个实际依赖点。
- base64 膨胀约 4/3。建议以原始字节 ≤ 3.75 MB 做预估，编码后再按编码后长度兜底一次；不要只按原始字节判断。

## 三、各 IM 通道的媒体能力

### 3.1 出站与入站一览

| 通道 | 出站媒体 | 入站媒体 | 关键限制 |
| --- | --- | --- | --- |
| Telegram【原文】 | `sendPhoto` / `sendDocument` / `sendVoice` / `sendAudio` | `photo` / `document` / `voice` / `audio` | 上传 10 MB（照片）/ 50 MB（其他）；URL 5 MB / 20 MB；`file_id` 无大小限制；下载经 `getFile` 最大 20 MB；`sendVoice` 上传接受 OGG/Opus、MP3、M4A，URL 方式限 `audio/ogg` ≤ 1 MB |
| Discord【原文】 | `POST /channels/{id}/messages`，`multipart/form-data` + `files[n]` + `payload_json` + `attachments` | 消息对象 `attachments`：`url`、`content_type`、`size`、`filename` | 单请求 25 MiB；正文 2000 字符 |
| Slack【原文】 | `files.getUploadURLExternal` → POST 到 `upload_url` → `files.completeUploadExternal` | `files` 数组：`url_private`、`mimetype`、`size`、`filetype` | 单文件 1 GB；`url_private` 下载要带 bot token |
| 飞书 / Lark【原文】 | `im/v1/images`（10 MB）→ `image_key`；`im/v1/files`（30 MB）→ `file_key`；消息 `msg_type` 取 `image` / `file` / `audio` / `media` | 消息内容里的 `image_key` / `file_key`；`im/v1/messages/{id}/resources/{key}?type=image|file` 下载 | 文本请求体 150 KB；图片 10 MB；文件 30 MB；资源下载 100 MB |
| QQ【原文】 | 先上传拿 `file_info`，再 `msg_type=7` + `media.file_info`；支持 URL 上传与分片上传 | 事件 `attachments`：`url`、`content_type`、`size`、`filename`、`voice_wav_url`、`asr_refer_text` | 软限：图片 20 MB、视频 30 MB、语音 20 MB、文件 200 MB；硬限 200 MB；超软限降级为文件；单聊被动回复 60 分钟 / 4 次，群聊 5 分钟 / 5 次 |
| 钉钉【原文】 | `batchSend` 的 `msgKey`：`sampleImageMsg`、`sampleAudio`、`sampleFile`、`sampleVideo`；先 `oapi.dingtalk.com/media/upload` 拿 `media_id` | 回调 `msgtype` 取 `picture` / `audio` / `video` / `file`，用 `downloadCode` 换临时下载链接 | 图片 20 MB；语音 2 MB（ogg/amr）；视频 20 MB（mp4）；文件 20 MB；`userIds` 文档写每次最多 20、错误码表写 100，按 20 保守处理 |
| 企业微信【原文】 | `aibot_respond_msg` / `aibot_send_msg` 支持 `image` / `file` / `voice` / `video`，先用 `media_id`；上传走 `aibot_upload_media_init` → `chunk` → `finish` 分片 | 回调 `image` / `file` / `video` 给 `url` + `aeskey`（5 分钟有效）；AES-256-CBC、PKCS#7 填充、IV 取 key 前 16 字节；语音给转写文本 | 图片 10 MB（png/jpg/gif）；语音 2 MB（amr）；视频 10 MB（mp4）；文件 20 MB；分片 ≤ 512 KB、最多 100 片；30 次/分钟、1000 次/小时；素材 3 天；回复窗口 24 小时 |
| 微信 iLink【推断】 | 代码只实现文本；`item_list` 的 type 注释为 1 文本 / 2 图片 / 3 语音 / 4 文件 / 5 视频 | 只实现文本与语音转写；图片/文件/视频的字段结构未移植 | 无公开文档；协议对照的是官方渠道包，媒体字段必须抓包或真机验证 |

### 3.2 结论

- **P1-3 第一批做 Telegram、Discord、Slack、飞书/Lark。** 这四条文档完整、上传下载闭环清楚，且覆盖了三种典型模型：URL 拉取、multipart 直传、两步上传。
- **第二批做 QQ、钉钉、企业微信。** 三者都是「先上传拿 key，再发消息」，可共用一个 `Uploader` 抽象；企业微信额外有 AES 解密与 5 分钟 URL，必须流式下载，不能先落盘再慢慢解。
- **微信 iLink 放最后。** 没有公开文档，媒体字段属于反推；在拿到真实账号并抓包确认 `item_list` 结构前，不要写进正式能力表。
- 现有 `imbot.Sender` 只有 `Send(ctx, userID, text, replyTo)`，且所有通道都把非文本入站归一成 `Note`「暂不支持」。P1-3 需要两层新抽象：入站的 `Attachment`（字节、MIME、文件名、时长、平台凭据）与出站的 `SendMedia`（图片/文件/语音），不能继续往 `text` 里塞路径。
- 安全边界：所有入站 URL 都必须走现有的受控 HTTP 客户端，限制大小、超时、重定向与内网地址；企业微信的 AES 解密要在内存里做，密钥不落盘。

## 四、语音：TTS / ASR

### 4.1 OpenAI【原文】

- **TTS 正在退役。** `tts-1`、`tts-1-hd`、`gpt-4o-mini-tts` 将于 **2027-01-06** 关闭；官方要求迁移到 Realtime API 的 `gpt-realtime-2.1-mini`，并明确它不是 `/v1/audio/speech` 的模型。现有 Speech API 只作为兼容窗口保留。
- Speech API 现状：11 个内置音色，`response_format` 支持 mp3 / opus / aac / flac / wav / pcm；默认 mp3，低延迟推荐 wav / pcm；`instructions` 可控制语气；`input` 有长度上限，长文本要自行切段【推断：具体字符上限未在本次页面读到】。
- **ASR 文件接口仍可用。** `/v1/audio/transcriptions` 推荐模型 `gpt-transcribe`；文件最大 25 MB，格式 mp3 / mp4 / mpeg / mpga / m4a / wav / webm；返回 `text` 与检测到的 `languages`；`prompt` / `keywords` / `languages` 可给上下文。`whisper-1`、`gpt-4o-transcribe` 系列与 `/v1/audio/translations` 将于 **2027-02-26** 关闭；时间戳只有 `whisper-1` 支持，也随之下线。
- 实时音频（麦克风、通话、媒体流）走 Realtime transcription，不是文件接口。

### 4.2 Anthropic【原文】

- API 概览只列 Messages、Batches、Token Counting、Models、Files、Skills（beta 另有 Agents/Sessions/Environments），**没有 TTS、ASR 或 embeddings 端点**。

### 4.3 对本项目的影响

- **入站语音优先用平台自带转写**：企业微信、钉钉、微信、QQ 都已经在回调里给转写文本（QQ 还有 `asr_refer_text` 与 `voice_wav_url`）。这条路径零模型成本、零格式转换，应该作为 P1-4 的默认。
- 只有 Telegram / Discord / Slack / 飞书需要自己下载音频再 ASR。文件接口 25 MB 够用，但要注意 Telegram 下载上限 20 MB、飞书资源 100 MB；超过 25 MB 的音频要先切段或压缩。
- **TTS 不要建在 Speech API 上。** 要么接 Realtime API（WebSocket/WebRTC，工作流完全不同），要么先不做。若只为短期兼容而接，代码里必须把「TTS 提供商」独立出来，2027 年前可替换。
- 各通道对音频格式的要求不一致：企业微信只要 amr、钉钉 ogg/amr、飞书 `audio` 要 opus；Telegram `sendVoice` 的上传接受 OGG/Opus、MP3、M4A，只有 URL 方式才限 `audio/ogg` 且 ≤ 1 MB（1–20 MB 会按文件发）。Go 标准库不能编码 amr/opus/mp3/m4a；如果只做各通道原生格式，Telegram 的 MP3/M4A 可以不转码，ffmpeg 的阻塞主要来自企业微信 amr、飞书 opus 与 TTS 输出格式。**P1-4 需要先决定「带 ffmpeg」还是「只支持各通道原生格式」。**
- OpenAI 的使用政策要求向最终用户明确告知 TTS 是 AI 生成的声音；如果做语音回复，文案与开关要体现这一点。

## 五、向量化

### 5.1 OpenAI【原文】

- `POST /v1/embeddings`；`input` 可以是字符串或字符串数组；`model` 取 `text-embedding-3-small` / `text-embedding-3-large`（`ada-002` 仍在表里）。
- 最大输入 8192 token；`text-embedding-3-small` 默认 1536 维、`large` 默认 3072 维；`dimensions` 可降维（仅 3 代及以后）；`encoding_format` 取 `float` 或 `base64`。
- 计费按输入 token；返回 `data[].embedding`、`model`、`usage`。

### 5.2 Anthropic【原文】

- 官方明确：「Anthropic does not offer its own embedding model.」推荐 Voyage AI（MongoDB）。
- 文档列出的 Voyage 4 代模型：`voyage-4-large` / `voyage-4` / `voyage-4-lite` / `voyage-code-4`，上下文 32000，默认 1024 维，可选 256 / 512 / 2048；另有 `voyage-4-nano`（2048 默认，可 256/512/1024），开源权重、需自行部署。

### 5.3 对本项目的影响

- **embedding 必须是独立配置项**，不能复用聊天模型的 provider/model。聊天走 Anthropic 的用户仍可能用 OpenAI 或 Voyage 做嵌入；反过来也一样。`models.json` 需要一个 `embedding` 选择（提供商、模型、维度、密钥），并在设置页单独展示。
- 索引里要存 `model`、`dimensions`、`text_hash` 与向量；换模型或改维度必须整体重建，不能混用。
- 记忆插件现在的 `pickRecalls` 是关键词打分，升级路径应是「关键词召回 + 向量重排」而不是纯向量：关键词对小语种、专有名词仍更稳，且向量调用有成本与隐私代价。
- 隐私边界：嵌入会把记忆正文发给第三方。必须默认关闭、由用户显式选择提供商；本地模型（如 `voyage-4-nano` 自部署）是后续可选项。
- 成本控制：写入时批量嵌入、查询时只嵌入本轮输入；不要每轮重嵌全部记忆。

## 六、尚需真机验证的清单

1. **每个通道各做一次媒体往返**：发图/发文件/发语音/发视频，收图/收文件/收语音/收视频，确认字段名、MIME、大小与失败错误码与文档一致。
2. **企业微信**：分片上传 100 片边界、512 KB 片大小、30/1000 频率限制、AES 解密后的字节是否与原始文件一致。
3. **QQ**：`file_info` 的 TTL 实际值、单聊/群聊上传接口不互通的边界、超软限降级行为、`asr_refer_text` 是否总存在。
4. **钉钉**：`batchSend` 的 `userIds` 到底 20 还是 100；`sampleAudio` 的 ogg/amr 与 `media/upload` 的 voice 类型是否要求同一格式。
5. **微信 iLink**：抓包确认 `item_list` 中图片/文件/视频的字段结构、下载是否同样带密钥/过期；在此之前不排期。
6. **OpenAI**：用真实 key 各打一次 `/v1/responses`（`store:false` + 工具 + 推理）、`/v1/embeddings`、`/v1/audio/transcriptions`，确认错误形状、流事件顺序与 `encrypted_content` 回传要求。
7. **兼容提供商**：逐家确认是否实现 `/v1/responses`（不能只看「OpenAI 兼容」）；不支持的提供商必须显式拒绝，而不是运行时 404。
8. **TTS 替代路径**：用 Realtime API 做一次最小语音合成，确认 WebSocket 事件、音频格式与延迟；同时评估 ffmpeg 依赖的分发成本。

## 七、对 P1 后续的直接影响

- **P1-2（Responses）**：先做协议适配层与 Item 持久化，再做设置页开关；默认仍用 Chat。备用链要能跨协议切换。
- **P1-3（附件与视觉）**：先定 `llm` 的 content part 与 `imbot.Attachment`，再按通道文档完整度分批；先做四条文档完整的通道。
- **P1-4（语音）**：先接平台转写（零成本），文件 ASR 作补充；TTS 暂缓，除非 Realtime 适配或 ffmpeg 方案先定。
- **P1-5（语义记忆）**：先做独立 embedding 配置与索引格式，再做混合召回；默认关闭、显式授权。

## 参考

- OpenAI Responses 迁移指南：https://platform.openai.com/docs/guides/migrate-to-responses
- OpenAI Responses 流式事件参考：https://developers.openai.com/api/docs/api-reference/responses-streaming
- OpenAI 推理与无状态回放：https://platform.openai.com/docs/guides/reasoning
- OpenAI 函数调用：https://platform.openai.com/docs/guides/function-calling
- OpenAI 视觉：https://platform.openai.com/docs/guides/images-vision
- OpenAI 语音合成：https://platform.openai.com/docs/guides/text-to-speech
- OpenAI 文件转录：https://platform.openai.com/docs/guides/speech-to-text
- OpenAI 向量：https://platform.openai.com/docs/guides/embeddings
- OpenAI 官方 Python SDK：https://github.com/openai/openai-python
- Anthropic 视觉：https://platform.claude.com/docs/en/build-with-claude/vision
- Anthropic 向量：https://platform.claude.com/docs/en/build-with-claude/embeddings
- Anthropic API 概览：https://platform.claude.com/docs/en/api/overview
- Anthropic 官方 Go SDK：https://github.com/anthropics/anthropic-sdk-go
- Telegram Bot API：https://core.telegram.org/bots/api
- Discord Message 资源：https://docs.discord.com/developers/resources/message
- Slack 文件上传：https://docs.slack.dev/reference/methods/files.getUploadURLExternal
- Slack File 对象：https://docs.slack.dev/reference/objects/file-object
- 飞书发送消息：https://open.feishu.cn/document/server-docs/im-v1/message/create
- 飞书消息内容结构：https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/im-v1/message/create_json
- 飞书获取消息资源：https://open.feishu.cn/document/server-docs/im-v1/message/get-2
- 飞书上传图片：https://open.feishu.cn/document/server-docs/im-v1/image/create
- 飞书上传文件：https://open.feishu.cn/document/server-docs/im-v1/file/create
- QQ 消息类型：https://bot.q.qq.com/wiki/develop/api-v2/server-inter/message/type/overview.html
- QQ 富媒体：https://bot.q.qq.com/wiki/develop/api-v2/server-inter/message/rich-media.html
- QQ 单聊消息事件（附件字段）：https://bot.q.qq.com/wiki/develop/api-v2/autogen/event/c2c_message_create.html
- 钉钉机器人消息类型：https://open.dingtalk.com/document/development/robot-message-type
- 钉钉批量发送：https://open.dingtalk.com/document/development/chatbots-send-one-on-one-chat-messages-in-batches
- 钉钉上传媒体：https://open.dingtalk.com/document/development/upload-media-files
- 企业微信智能机器人长连接：https://developer.work.weixin.qq.com/document/path/101463
