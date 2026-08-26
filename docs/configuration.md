# 配置与模型

[← 返回 README](../README.md)　·　中文　·　[English](en/configuration.md)

## 配置说明

见 [config.example.yaml](../config.example.yaml)。要点：

| 配置项 | 说明 |
|---|---|
| `lang` | 界面与运行信息的语言：`zh` / `en`，不填则跟随系统区域（见下） |
| `model.provider` / `model.name` | 初始使用的 Provider 与模型 id（之后由设置页的「模型」栏目接管，见下） |
| `model.thinking` | 思考模式：`off` / `low` / `medium` / `high` / `xhigh` / `max`，默认 `high`（开启时 `temperature` 不生效；思考过程在 Web UI 以可折叠块展示） |
| `model.context_length` | 模型上下文窗口（token），默认 1000000；超出预算时自动从最旧对话轮次整轮裁剪；会话实测占用达到 90% 时自动压缩为摘要 |
| `server.trust_loopback` | 回环来源是否免认证，默认开启；套反向代理时必须关掉（见[部署与访问控制](deployment.md)） |
| `providers.<name>` | Provider 注册表：`type`（`openai_compat` / `anthropic`）、`base_url`、`api_key` |
| `agent.system_prompt` | 系统提示词（当前留空；运行时会自动在其前注入 `[系统环境]` 块：操作系统、Shell、工作目录、区域语言） |
| `agent.max_turns` | 单次请求内工具循环上限 |
| `agent.workdir` | 插件与环境块共用的工作目录，空 = 进程当前目录 |
| `session.dir` | 会话存储目录，默认 `<配置目录>/sessions` |

插件的开关与配置**不在 config.yaml 里**：它们由设置页维护、只存 `<配置目录>/plugins.state.json`。首次运行时按各插件声明的默认参数启用，以下十个除外：

- `read_file`、`exec_command`——能读任意文件、执行任意命令。这两样是「拿它当干活的 Agent」时才要的东西，要用就去设置页打开，那是个看得见的选择。
- `dual_persona`（不填触发词就切不过去）、`weather`（要填城市，且要联网）、`skills`（技能目录是空的，开着只多两个用不上的工具）。
- `qq_bot`、`wechat_bot`、`feishu_bot`、`lark_bot`、`telegram_bot`——不填凭证或不扫码绑定就没法工作。

其余全部默认启用，**包括整个角色演绎组与 `heartbeat`**——开箱就该是这个项目本来的样子，见[开箱形态](plugins/README.md)。心跳默认开着意味着装上即按固定节奏消耗额度，不需要就在设置页关掉它。插件自身的数据落在 `<配置目录>/plugins/<插件名>/`。

（早期版本支持在 config.yaml 写 `plugins:` 段。两处都能配的时候，哪一份在生效需要记住一条优先级规则，而设置页的改动又不回写配置文件，于是文件里的内容会慢慢变成误导。现在该段已不再生效，启动时会提示一句，可以直接删掉。）

## 语言

`lang:` 决定**这个服务**用哪门语言：发给模型的提示词与工具说明、插件的状态行、写进会话的注记、运行日志，以及 `wen status`、`wen plugins` 这类命令的输出。取值 `zh` / `en`；不填则跟随系统区域，判不出来时用中文。填了个不认识的语言名按没填算，不会让服务起不来。启动日志与 `wen status` 各打一行说明当前是哪门语言、是配的还是猜的。

它只能手改 config.yaml——设置页不写这一项，理由与模型配置相反：`lang` 只有一个值、几乎不会改，而配置文件从不回写（回写会固化 `${VAR}` 的明文并丢掉注释）。

**浏览器界面的语言是另一件事**，在设置页右上角选（`跟随浏览器 / 中文 / English`），只存在浏览器里。同一个服务可能一台电脑一台手机各用一种语言，而 `lang:` 是共享的，所以这两件事分开：一个管这张网页，一个管发给模型的与写进日志的。

**换语言不改写已经存下来的东西。** 旧的压缩摘要、记忆条目、日程与会话注记仍是写入时那门语言——它们是当时发生的事实，不是界面文案。

## 模型配置（设置页 → 模型）

提供商与模型可以在 Web UI 的设置页里增删改，并切换当前使用的组合，保存后**下一次对话请求即生效**，无需重启。

- 改动写入 `<配置目录>/models.json`（权限 0600，已在 `.gitignore` 中），**不回写 config.yaml**——因为 `api_key` 里的 `${VAR}` 在解析前就被展开，回写会把明文密钥固化进文件并丢掉全部注释。
- config.yaml 的 `providers:` 与 `model:` 段是初始值：同名条目由 models.json 完全覆盖；config.yaml 里**新增**的提供商仍会出现在列表中（标记「来自配置文件」），在界面上删除它会记一条删除坠碑，重启不会复活；从未在界面上改过的条目不写进 models.json，继续跟随配置文件。
- 每个模型条目可单独覆盖 `context_length` / `max_tokens` / `thinking` / `temperature`，留空则回退到 `model:` 段的全局值。
- **注意**：`${VAR}` 展开后的密钥一旦被写进 models.json 就固定下来，此后修改环境变量不再生效。

支持两种 API 模式：

| 模式 | 说明 |
|---|---|
| `openai_compat` | OpenAI 兼容协议（DeepSeek 等） |
| `anthropic` | Anthropic Messages API。思考档位映射为 `thinking:{type:"adaptive"}` + `output_config.effort`（`off` 则为 `disabled`）；当前世代 Claude 模型不接受采样参数，故不发送 `temperature`，模型条目里的该项对 Anthropic 无效 |

Anthropic 模式下多一个**提示词缓存**开关（默认开启）：开启时把请求里不变的部分（工具、system、已有历史）标为可缓存，命中的部分按约十分之一的价格计费。代价是未命中时的写入要多付约四分之一，而缓存有效期只有几分钟——如果你的对话节奏比这更慢（比如通过 IM 断断续续地聊），关掉更省。OpenAI 兼容模式没有这个开关：DeepSeek 与 OpenAI 的缓存都是服务端自动维护的，命中与否只取决于请求前缀稳不稳定（见[上下文的组织](context.md)）。

