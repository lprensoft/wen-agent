# HTTP API

[← 返回 README](../README.md)　·　中文　·　[English](en/http-api.md)


| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/sessions` | 会话列表 |
| POST | `/api/sessions` | 新建会话 |
| GET | `/api/sessions/{id}` | 会话历史消息 |
| DELETE | `/api/sessions/{id}` | 删除会话 |
| POST | `/api/chat` | `{"session_id","message"}` → SSE 流（`delta` / `thinking` / `tool_start` / `tool_result` / `confirm_request` / `confirm_done` / `compact_*` / `done` / `error`） |
| POST | `/api/sessions/{id}/compact` | 手动压缩该会话的历史（SSE 流，帧同 `/api/chat` 的 `compact_*`） |
| POST | `/api/confirmations/{id}` | `{"approved": bool}` 回答一次操作确认（id 取自 `confirm_request` 帧）。已超时或已回答过时返回 409 |
| GET | `/api/plugins` | 插件列表与状态（含来源 `source`、可配置项声明 `config_fields` 与当前生效值 `config`） |
| PUT | `/api/plugins/{name}` | `{"enabled": bool}` 运行时开关插件 |
| PUT | `/api/plugins/{name}/config` | `{"config": {...}}` 保存插件配置，校验通过后立即生效并持久化 |
| POST | `/api/plugins/{name}/actions/{key}` | 触发插件声明的操作（如微信扫码绑定），立即返回，流程在后台跑。请求体可选：`{"config": {...}, "token": "..."}`，`config` 是配置弹窗里尚未保存的值，`token` 原样带回列表里该操作的 `token`（按钮指的是哪一个，如批准的是哪一条提议） |
| GET | `/api/plugins/{name}/actions/{key}` | 轮询该操作的进展：状态、说明文字与一张可选 PNG（只经内存下发，不落盘） |
| GET | `/api/status` | 模型配置与插件状态行；带 `session_id` 查询参数时附上该会话的用量 |
| GET | `/api/events` | 常驻 SSE 流，推送会话注记（后台任务在会话里留下的、模型看不到的一行说明） |
| GET | `/api/models` | 提供商与模型配置（`api_key` 只返回掩码） |
| PUT | `/api/models` | 整档保存；请求里 `api_key` 留空表示不修改 |
| PUT | `/api/models/current` | `{"provider","model"}` 切换当前模型，立即热生效 |
| POST | `/api/models/test` | 用一次极小的真实请求测试连接 |
| GET | `/api/auth/status` | 访问控制状态（是否已设口令、当前请求是否已认证、是否对外监听），登录前即可访问 |
| POST | `/api/auth/login` | `{"password"}` 登录，成功后下发会话 Cookie |
| POST | `/api/auth/logout` | 退出登录 |
| PUT | `/api/auth/password` | `{"current","new"}` 设置或清除访问口令；`new` 留空表示清除（仅在只监听本机时允许） |

