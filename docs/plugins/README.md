# 插件总览

[← 文档首页](../README.md)　·　[← 返回项目 README](../../README.md)　·　中文　·　[English](../en/plugins/README.md)

核心（agent / session / server / llm）不含任何具体工具，能力一律由插件提供。插件的开关与配置在 Web UI 的设置页（或远程机器上的 `wen config plugins`）里维护，改动即时生效，不写进 config.yaml。

插件里可编辑的提示词（心跳提示词、兜底台词这些）**默认值跟着界面语言给出**：英文界面下文本框预填的是英文版，点「恢复默认」也回到英文版。插件注入给模型的固定提示词仍然是中文。

| 分组 | 插件 | 一句话 | 默认 |
|---|---|---|---|
| [基础工具](tools.md) | `read_file` | 读取本地文本文件 | 开 |
| | `web_fetch` | 抓取网页正文 | 开 |
| | `exec_command` | 执行命令，破坏性操作执行前拦截并交人确认 | 开 |
| [记忆与检索](memory.md) | `memory` | 跨会话的长期记忆：自动提炼、矛盾修订、按日时间线、可选淡忘 | 开 |
| | `session_search` | 按关键词与日期回查原始对话，含压缩前的完整归档 | 开 |
| | `skills` | 按需读取的技能手册（SKILL.md） | 关 |
| [角色演绎](roleplay.md) | `roleplay` | 角色设定、台词样例、自然表达与时间一致性约束 | 关 |
| | `dual_persona` | 表里两套人格，由触发词切换，记忆与历史互不可见 | 关 |
| [角色的生活](life.md) | `scene` | 舞台设定与场景记忆 | 关 |
| | `weather` | 按城市取现实天气，作为环境状态每轮注入 | 关 |
| | `belongings` | 冰箱、衣柜里有什么 | 关 |
| | `people` | 角色的社交圈：朋友、家人与亲近度 | 关 |
| | `agenda` | 每天给自己排一张表，到点去做；约定与「答应过的事」台账 | 关 |
| | `relationship` | 关系阶段、称呼、默契与禁区 | 关 |
| | `unspoken` | 没说出口的心里话 | 关 |
| | `body_sense` | 按部位的接触熟悉度，以及唤起与疲劳 | 关 |
| | `mood` | 心情随互动增减、随时间回落 | 关 |
| | `health` | 偶尔的日常小恙，病程随时间起落 | 关 |
| | `presence` | 此刻的所在、穿着、姿态与正在做的事 | 关 |
| | `style_watch` | 用正则规则量出「助手腔」的出现频率 | 关 |
| [后台任务](background.md) | `heartbeat` | 心跳：按间隔主动开口，节奏由角色自己定 | 关 |
| | `scheduler` | 定时任务（一次性 / 周期 / 每日） | 开 |
| [消息通道](im.md) | `qq_bot` | QQ 官方开放平台 | 关 |
| | `wechat_bot` | 微信官方 ClawBot 插件（扫码绑定） | 关 |
| | `feishu_bot` | 飞书（中国版） | 关 |
| | `lark_bot` | Lark（国际版） | 关 |
| | `telegram_bot` | Telegram Bot API | 关 |
| [程序维护](self-update.md) | `self_update` | 查新版、一键下载校验替换并重启 | 开 |
| [外源插件](external.md) | `bundles` | 装 Agent Plugins 格式的插件包 | 开 |
| | `ext_*` | 装进来的每个插件包各占一张卡片 | 开 |

要自己写一个插件，见[项目结构与插件开发](../architecture.md)。
