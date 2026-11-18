# HTTP API

[← 返回 README](../README.md)　·　中文　·　[English](en/http-api.md)


| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/sessions` | 会话列表 |
| POST | `/api/sessions` | 新建会话 |
| GET | `/api/sessions/{id}` | 会话历史消息（图片只给 `name` / `media_type` / `size`） |
| GET | `/api/sessions/{id}/images/{name}` | 返回会话里一张聊天图片的原始字节 |
| DELETE | `/api/sessions/{id}` | 删除会话 |
| POST | `/api/chat` | `{"session_id","message","images"}` → SSE 流（`delta` / `thinking` / `tool_start` / `tool_result` / `confirm_request` / `confirm_done` / `compact_*` / `done` / `error`）。`images` 可省略，每项是 `{"media_type","data"}`，`data` 为 base64 |
| POST | `/api/sessions/{id}/compact` | 手动压缩该会话的历史（SSE 流，帧同 `/api/chat` 的 `compact_*`） |
| POST | `/api/confirmations/{id}` | `{"approved": bool}` 回答一次操作确认（id 取自 `confirm_request` 帧）。已超时或已回答过时返回 409 |
| GET | `/api/plugins` | 插件列表与状态（含来源 `source`、可配置项声明 `config_fields` 与当前生效值 `config`） |
| PUT | `/api/plugins/{name}` | `{"enabled": bool}` 运行时开关插件 |
| PUT | `/api/plugins/{name}/config` | `{"config": {...}}` 保存插件配置，校验通过后立即生效并持久化 |
| POST | `/api/plugins/{name}/actions/{key}` | 触发插件声明的操作（如微信扫码绑定），立即返回，流程在后台跑。请求体可选：`{"config": {...}, "token": "..."}`，`config` 是配置弹窗里尚未保存的值，`token` 原样带回列表里该操作的 `token`（按钮指的是哪一个，如批准的是哪一条提议） |
| GET | `/api/plugins/{name}/actions/{key}` | 轮询该操作的进展：状态、说明文字与一张可选 PNG（只经内存下发，不落盘） |
| POST | `/api/plugins/{name}/state/edit` | 状态页上的一次修正：`{"domain","section","row","op","values","originals"}`。`originals` 是表单打开时各字段的值，服务端据此只写用户真正改过的字段（旧客户端不带时按全部提交处理）。各字段都是客户端提交的，插件只认磁盘上真有的可见域、只按自己的数据核对行，并经自己的存储接口落盘；错误按请求语言原样返回。请求体上限 64KB |
| GET | `/api/status` | 模型配置与插件状态行；带 `session_id` 查询参数时附上该会话的用量 |
| GET | `/api/state` | 启用插件的状态快照（记忆索引、日记、今日安排、现场等），按功能分组；带 `lang` 查询参数；`pinned=1` 只返回置顶块。行与块可带 `edits`（可修改项）与 `id`（行身份），`pinned` 响应里会裁掉。内容含记忆、日程、关系、心情、健康、现场与心里话这类私人文本，响应带 `Cache-Control: no-store`，不进入模型上下文 |
| GET | `/api/events` | 常驻 SSE 流，推送两类事件：`notice`（后台任务在会话里留下的、模型看不到的一行说明）与 `turn`（一轮结束，只带会话 id、时间与一小段预览，界面据此标未读、在页面不可见时弹桌面通知） |
| GET | `/api/models` | 提供商与模型配置（`api_key` 只返回掩码） |
| PUT | `/api/models` | 整档保存；请求里 `api_key` 留空表示不修改 |
| PUT | `/api/models/current` | `{"provider","model"}` 切换当前模型，立即热生效 |
| POST | `/api/models/test` | 用一次极小的真实请求测试连接 |
| GET | `/api/embedding` | 嵌入服务配置（`api_key` 只返回掩码），并带上接口类型与预置 |
| PUT | `/api/embedding` | 保存嵌入服务配置，立即生效；`api_key` 留空且地址没变时表示不修改 |
| POST | `/api/embedding/test` | 用一次真实的嵌入请求测试表单里的配置，返回 `{"ok":true,"dimensions":N}` |
| GET | `/api/auth/status` | 访问控制状态（是否已设口令、当前请求是否已认证、是否对外监听），登录前即可访问 |
| POST | `/api/auth/login` | `{"password"}` 登录，成功后下发会话 Cookie |
| POST | `/api/auth/logout` | 退出登录 |
| PUT | `/api/auth/password` | `{"current","new"}` 设置或清除访问口令；`new` 留空表示清除（仅在只监听本机时允许） |

聊天图片一次最多 4 张、单张不超过 5 MB、总计不超过 10 MB；只接受 PNG、JPEG 与 GIF；类型与可解码性按文件字节判断，不信任请求里声明的 MIME。图片写入 `<会话目录>/<会话 id>/images/`，JSONL 里只保存文件名引用，删除会话时一并删除。

