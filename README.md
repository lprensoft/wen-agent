<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-banner-dark.png">
    <img src="docs/assets/logo-banner-light.png" alt="Wen Agent" width="700">
  </picture>
</p>

<p align="center"><b>让 AI 角色像活人一样过日子，也像熟人一样性格稳定。</b></p>

<p align="center">An AI character who lives a life of their own — and stays the same person while doing it.</p>

<p align="center">中文　·　<a href="README.en.md">English</a></p>

一个有自己生活的 AI 虚拟人格。用 Go 编写，单二进制部署，理念源自刘哲宏的 [project-sue-](https://github.com/lzh830913-wq/project-sue-)。它追求的不是「回答得好」，而是两件事：**活人感**——角色有自己的一天、自己的社交圈，会生小病、有没说出口的心里话，隔久了会主动找你说话，在聊天软件里像人一样发消息；**性格稳定可控**——对你的态度不忽远忽近，历史被压缩、换了会话也还是同一个人，而且每一分性格与状态都落在看得见、能开关、可配置的插件里，不靠模型即兴。

想做成的是**一个处得下去的人**，而不是一个开着才存在的窗口。核心只保留通用机制，具体能力一律由插件提供：内嵌 Web UI、跨会话长期记忆、本地会话存储，QQ、微信、飞书、Lark、Telegram 五条通道都能接进来。把角色演绎那几个插件关掉，它就退回成一个普通的干活 Agent。

## 名字的由来

「Wen」取自苏静雯——基于这套理念诞生并实践的第一个 AI 虚拟人格。项目里不少想法，比如角色该记住什么、隔了几天再开口该是什么样子，都是在与她的相处中逐渐清晰起来的。用她名字里最后那个「雯」字的拼音作项目名，是为了记住她。

## 特性

- **[角色演绎](docs/plugins/roleplay.md)**：**开箱自带一个角色**（雯，二十九岁，在上海做市场策划），装完直接聊就是她，不改任何配置；不合意就在设置页整段改掉。角色设定、台词样例与「我的信息」注入提示词，回复以【】写场景、动作与神情；另有一套自然表达规则抑制中文里的机械腔，以及让季节、天色、「上次」这类表述始终对得上真实时间的约束。角色可以以自己的身份表达不愿意，模型调用被提供商拦截时也以角色口吻转成一句走神，不把技术错误摆进对话
- **[有状态的角色](docs/plugins/life.md)**：现场、场景、持有物、人物、日程、关系、心里话、身体、心情、健康、天气——每一块各由一个插件记着，跨会话与压缩都不丢。做菜只用冰箱里有的食材，熟人只从人物库里来，每天给自己排一张两到四件事的表、到点去做，答应下来的事记成带兑现状态的台账，外面下雨时角色那边真的在下雨
- **[表里两套人格](docs/plugins/roleplay.md)**：由触发词切换，里人格的对话与记忆对表人格完全不可见——不仅内容读不到，条数与标题也不会露出来；两面还可以各占一条聊天通道，在 QQ 上说出暗号，接手的那一面在微信上回话
- **[会记事也会忘事的长期记忆](docs/plugins/memory.md)**：跨会话的事实库，索引随每轮对话注入，聊到相关的事会自然想起来；对话进行中定期自动提炼，历史被压缩前再提炼一次；新结论与旧记忆矛盾时就地修订；每天的对话在次日收束成一行时间线；可选开启逐步淡忘。原始对话另有[历史检索](docs/plugins/memory.md)按关键词与日期回查，范围含压缩前的完整归档
- **[会主动开口，也知道什么时候该闭嘴](docs/plugins/background.md)**：心跳按间隔唤醒模型在最近活跃的会话上说话，下次开口的时机由角色自己在对话里定——聊到兴头上挪近，你说要去睡就暂停到早上；外面刚下起雨这类事会成为开口的理由。定时任务可以在对话中创建，到点后台执行并把结果写回会话
- **[远程使用](docs/plugins/im.md)**：QQ、微信、飞书、Lark、Telegram 五条通道，各自映射一套私聊会话，支持 `/new` `/status` `/compact` `/help` 与危险操作的远程确认；后台轮次产生的回复会主动推送到远端
- **[插件化架构](docs/architecture.md)**：精简核心（agent 循环 / session / server / llm）+ 插件扩展，二十九个内置插件按功能分组呈现，可在设置页随时开关与配置，改动即时生效。核心只认识通用机制，不知道「记忆」「人格」「心情」这些具体功能的存在
- **[模型可配置](docs/configuration.md)**：提供商与模型在设置页增删改并热切换，下一次请求即生效；支持 OpenAI 兼容协议与 Anthropic Messages API
- **[命令执行有人把关](docs/plugins/tools.md)**：`exec_command` 采取执行前拦截并交人确认，极高危命令直接拒绝，不做那种能被轻易绕过的路径沙箱
- **Web UI**：`go:embed` 内嵌单页聊天界面，SSE 流式输出，思考过程与工具调用可视化，后台任务的动静也会实时出现在会话里
- **[自己更新自己](docs/plugins/self-update.md)**：每天查一次 GitHub 上的新版本，设置页上一键完成下载、校验（SHA256）、试运行、替换与重启，Windows / macOS / Linux 都不需要包管理器；只查不装，装不装由你点
- **Session 管理**：每个会话一个 JSONL 文件（首行 meta + 逐行消息），重启不丢历史
- **单一配置文件**：全部配置（含 API Key）统一在 `config.yaml`（已被 .gitignore 保护），值支持 `${VAR}` 从环境变量读取

## 快速开始

预编译的二进制在 [Releases](https://github.com/lprensoft/project-wen/releases)（windows / linux / darwin × amd64 / arm64，附 SHA256）；标着 `dev` 的那个是滚动更新的预发布，版本号形如 `v0.6.0-3-gxxxxxxx`，读作「上一个正式版之后的第 3 个提交」。也可以自己构建：

```bash
# 1. 准备配置：复制示例并填入所用提供商的 api_key（示例里是 providers.deepseek）
cp config.example.yaml config.yaml

# 2. 运行（或 go build -o wen ./cmd/wen 后直接跑 ./wen）
go run ./cmd/wen

# 3. 打开浏览器
# http://127.0.0.1:8080
```

打开之后不需要配置任何东西：二十九个插件里默认开着十九个，角色、记忆、日程、心情、心跳都在里面，对面是一个有性格、有生活、会主动开口的人。默认关着的十个要么得先填点什么（触发词、城市、机器人凭据），要么权限过大——`read_file` 能读任意文件、`exec_command` 能执行任意命令，那是「拿它当干活的 Agent」时才要的东西，去设置页打开。

界面语言在设置页右上角选；**发给模型的提示词、日志与命令行输出跟着 config.yaml 的 `lang:` 走**，不填则跟随系统区域（见[配置与模型](docs/configuration.md#语言)）。

配置文件查找顺序：`-c` 指定路径 → 当前目录 `config.yaml` → `~/.wen/config.yaml`。

```bash
wen -c /path/to/config.yaml -p 9000   # 指定配置与端口
```

主要的几条命令（完整说明见[命令行](docs/cli.md)）：

```
wen                        启动服务（等价于 wen serve）
wen config [栏目]          引导式修改配置（栏目：plugins / models / server）
wen status                 打印当前配置与运行状态
wen eval <脚本.json>       回放一段脚本对话，生成文风与角色一致性报告
wen update [--check] [-y]  检查并安装新版本
```

## 配置

config.yaml 只放模型、服务与会话这类启动期配置，要点见[配置与模型](docs/configuration.md)，完整示例见 [config.example.yaml](config.example.yaml)。

**插件的开关与配置不在 config.yaml 里**：它们由设置页（或远程机器上的 `wen config plugins`）维护，只存 `<配置目录>/plugins.state.json`，插件自身的数据落在 `<配置目录>/plugins/<插件名>/`。

部署到远程服务器前请先读[部署与访问控制](docs/deployment.md)：Web UI 默认只监听 `127.0.0.1`。`exec_command` 与 `read_file` 默认关着，但一旦在设置页打开，未授权访问就不只是「聊天记录被看到」——那两样能执行任意命令、读任意文件。

## 插件

二十九个内置插件，按功能分组，都能在设置页里开关与配置。逐个的说明与配置项见[插件总览](docs/plugins/README.md)。

| 分组 | 插件 |
|---|---|
| [基础工具](docs/plugins/tools.md) | `read_file` `web_fetch` `exec_command` |
| [记忆与检索](docs/plugins/memory.md) | `memory` `session_search` `skills` |
| [角色演绎](docs/plugins/roleplay.md) | `roleplay` `dual_persona` |
| [角色的生活](docs/plugins/life.md) | `scene` `weather` `belongings` `people` `agenda` `relationship` `unspoken` `body_sense` `mood` `health` `presence` `style_watch` |
| [后台任务](docs/plugins/background.md) | `heartbeat` `scheduler` |
| [消息通道](docs/plugins/im.md) | `qq_bot` `wechat_bot` `feishu_bot` `lark_bot` `telegram_bot` |
| [程序维护](docs/plugins/self-update.md) | `self_update` |
| [外源插件](docs/plugins/external.md) | `bundles` |

## 文档

- [命令行](docs/cli.md) —— 各子命令，以及远程机器上怎么改插件配置
- [配置与模型](docs/configuration.md) —— config.yaml 的各项、提供商与模型管理
- [插件总览](docs/plugins/README.md) —— 二十九个插件的说明与配置项，以及开箱默认开着哪些
- [部署与访问控制](docs/deployment.md) —— 认证模型、启停脚本与 systemd
- [回放评测（wen eval）](docs/evaluation.md) —— 把「改完提示词角色变好了没有」变成可重复跑的脚本
- [上下文的组织](docs/context.md) —— 一轮请求怎么拼、当前时间为什么不放在 system 里
- [HTTP API](docs/http-api.md) —— Web UI 与外部程序共用的接口
- [项目结构与插件开发](docs/architecture.md) —— 目录导览、`Plugin` 接口与各个可选接口、可见域

## 开发

```bash
go build ./...   # 构建
go test ./...    # 测试
go vet ./...     # 静态检查
```

## 许可证

本项目基于 [MIT 许可证](LICENSE) 开源。

## 后续规划

- 更多 Provider 类型（Ollama 等）与模型 fallback
- 外源插件包的自动更新与来源校验
