# 配置与模型

[← 返回 README](../README.md)　·　中文　·　[English](en/configuration.md)

## 首次运行

新装的人不需要写任何配置文件。第一次运行 `wen` 时，如果还没有能用的模型，会先进入一个终端向导：

1. 选语言。界面、发给模型的提示词与运行日志都用它。
2. 选模型提供商。列表就是设置页「新增提供商」那张预置表（见下文[添加提供商](#添加提供商)），最后一项是「自定义」。
3. 填 API Key，并选一个模型。指向本机的提供商（如 Ollama）不要求密钥。
4. 试连一次。连不上可以重新填、换一家，或者照样保存。
5. 确认监听端口。默认 8080，被别的程序占用时会预先换成下一个空着的端口。

答完就接着启动服务。之后要换模型或加提供商，用设置页或 `wen config models`；想重走一遍向导就运行 `wen onboard`，它会新增一家提供商并切换过去，已有的不动。

向导的答案各归各的去处，不另开文件：

- 模型写进 `<配置目录>/models.json`，与设置页、`wen config` 改的是同一份。
- 端口与语言只在**与默认值不同**、且配置目录里还没有 `config.yaml` 时，才写进一份只含这几项的 `config.yaml`。都取默认值的话，就一个配置文件都没有。
- 已有的 `config.yaml` 向导从不改写。

不在终端里运行时（systemd、容器、`nohup`）没法提问，还没配模型的话会打一行提示后退出。这种部署先在终端里跑一次 `wen onboard`，或者照下文在 `config.yaml` 里写好 `model:` 与 `providers:` 两段。

另有两种情况即使在终端里也不进向导，而是直接报错，因为向导解决不了它们：

- 当前提供商写在 `config.yaml` 里、只是 `api_key` 为空。这几乎总是 `${VAR}` 引用的环境变量没设，报错会点名是哪个提供商。进向导的话只能再加一家提供商，并把明文密钥存进 `models.json`。
- 同一配置目录下已经有实例在跑。这时该用 `wen onboard` 或设置页，改动会经运行中的实例当场生效。

**配置目录**默认是 `~/.wen/`；当前目录下有 `config.yaml` 时改用当前目录；`-c` 指定的配置文件所在的目录也就是配置目录。模型配置、会话、插件状态与访问口令都放在这里。

## config.yaml（可选）

只有想改下面这些启动期配置时才需要它，放在配置目录里，只写要改的那几项，其余取默认值。早先写好的完整 `config.yaml` 照常生效。

| 配置项 | 说明 |
|---|---|
| `lang` | 这个服务用哪门语言：`zh` / `en`，不填则跟随系统区域（见下） |
| `server.host` | 监听地址，默认 `127.0.0.1`。改成对外地址前先设访问口令（见[部署与访问控制](deployment.md)） |
| `server.port` | 监听端口，默认 `8080`；命令行的 `-p` 优先于它 |
| `server.trust_loopback` | 回环来源是否免认证，默认开启；套反向代理时必须关掉（见[部署与访问控制](deployment.md)） |
| `model.provider` / `model.name` | 初始使用的提供商与模型 id（之后由设置页的「模型」栏目接管，见下）。不写时，若 `providers` 里有名为 `deepseek` 的条目，默认选它，模型为 `deepseek-flash`（旧版默认的 `deepseek-v4-flash` 已下线） |
| `model.temperature` | 默认 `0.7`；思考开启时不生效 |
| `model.max_tokens` | 单次回复的输出上限，默认 `4096` |
| `model.thinking` | 思考模式：`off` / `low` / `medium` / `high` / `xhigh` / `max`，默认 `high`（思考过程在 Web UI 以可折叠块展示） |
| `model.context_length` | 模型上下文窗口（token），默认 1000000；超出预算时自动从最旧对话轮次整轮裁剪；会话实测占用达到 90% 时自动压缩为摘要 |
| `providers.<name>` | 提供商注册表：`type`（`openai_compat` / `anthropic`）、`base_url`、`api_key`，可选 `thinking_dialect` 与 `prompt_cache`（见下） |
| `agent.system_prompt` | 系统提示词，默认留空（运行时会自动在其前注入 `[系统环境]` 块：操作系统、Shell、工作目录、区域语言） |
| `agent.max_turns` | 单次请求内工具循环上限，默认 `20` |
| `agent.workdir` | 插件与环境块共用的工作目录，空 = 进程当前目录 |
| `session.dir` | 会话存储目录，默认 `<配置目录>/sessions` |

`model.temperature`、`max_tokens`、`thinking`、`context_length` 四项同时是全局默认值：模型条目没有单独覆盖时回落到它们。

值里可以写 `${VAR}`，加载时替换成环境变量。容器这类没有终端的部署可以这样配模型，不必进向导：

```yaml
model:
  provider: deepseek
  name: deepseek-flash
providers:
  deepseek:
    type: openai_compat
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
```

插件的开关与配置**不在 config.yaml 里**：它们由设置页维护、只存 `<配置目录>/plugins.state.json`。首次运行时按各插件声明的默认参数启用，以下十个除外：

- `read_file`、`exec_command`——能读任意文件、执行任意命令。这两样是「拿它当干活的 Agent」时才要的东西，要用就去设置页打开，那是个看得见的选择。
- `dual_persona`（不填触发词就切不过去）、`weather`（要填城市，且要联网）、`skills`（技能目录是空的，开着只多两个用不上的工具）。
- `qq_bot`、`wechat_bot`、`feishu_bot`、`lark_bot`、`telegram_bot`——不填凭证或不扫码绑定就没法工作。

其余全部默认启用，**包括整个角色演绎组与 `heartbeat`**——开箱就该是这个项目本来的样子，见[开箱形态](plugins/README.md)。心跳默认开着意味着装上即按固定节奏消耗额度，不需要就在设置页关掉它。插件自身的数据落在 `<配置目录>/plugins/<插件名>/`。

（早期版本支持在 config.yaml 写 `plugins:` 段。两处都能配的时候，哪一份在生效需要记住一条优先级规则，而设置页的改动又不回写配置文件，于是文件里的内容会慢慢变成误导。现在该段已不再生效，启动时会提示一句，可以直接删掉。）

## 语言

`lang:` 决定**这个服务**用哪门语言：发给模型的提示词与工具说明、插件的状态行、写进会话的注记、运行日志，以及 `wen status`、`wen plugins` 这类命令的输出。取值 `zh` / `en`；不填则跟随系统区域，判不出来时用中文。填了个不认识的语言名按没填算，不会让服务起不来。启动日志与 `wen status` 各打一行说明当前是哪门语言、是配的还是猜的。

首次运行向导里选的语言与系统区域不同时，向导会把它写进新建的 `config.yaml`。此后只能手改这个文件——设置页不写这一项，理由与模型配置相反：`lang` 只有一个值、几乎不会改，而配置文件从不回写（回写会固化 `${VAR}` 的明文并丢掉注释）。

**浏览器界面的语言是另一件事**，在设置页右上角选（`跟随浏览器 / 中文 / English`），只存在浏览器里。同一个服务可能一台电脑一台手机各用一种语言，而 `lang:` 是共享的，所以这两件事分开：一个管这张网页，一个管发给模型的与写进日志的。

**换语言不改写已经存下来的东西。** 旧的压缩摘要、记忆条目、日程与会话注记仍是写入时那门语言——它们是当时发生的事实，不是界面文案。

## 模型配置（设置页 → 模型）

提供商与模型可以在 Web UI 的设置页里增删改，并切换当前使用的组合，保存后**下一次对话请求即生效**，无需重启。

- 改动写入 `<配置目录>/models.json`（权限 0600，已在 `.gitignore` 中），**不回写 config.yaml**——因为 `api_key` 里的 `${VAR}` 在解析前就被展开，回写会把明文密钥固化进文件并丢掉全部注释。
- config.yaml 的 `providers:` 与 `model:` 段（如果写了）是初始值：同名条目由 models.json 完全覆盖；config.yaml 里**新增**的提供商仍会出现在列表中（标记「来自配置文件」），在界面上删除它会记一条删除坠碑，重启不会复活；从未在界面上改过的条目不写进 models.json，继续跟随配置文件。
- 每个模型条目可单独覆盖 `context_length` / `max_tokens` / `thinking` / `temperature`，留空则回退到 `model:` 段的全局值。
- **注意**：`${VAR}` 展开后的密钥一旦被写进 models.json 就固定下来，此后修改环境变量不再生效。

### 添加提供商

设置页的「新增提供商」先给一张预置列表：挑一家，接口地址、思考参数方言与常用模型就都填好了，你只需要补一个 API Key，表单里还给出了去哪儿领它的链接。挑不到的话，列表最后一项是「自定义」，走原来那套手工填写。`wen config` 的终端界面同样是这个流程。

预置只是**填表单的初值**，不是第三种配置来源：保存下来的就是一条普通的用户条目，此后与预置表再无关系——程序升级后表里多了个模型，你这条不会自己长出来，也不会被改回去。这是刻意的，理由与 config.yaml 和 models.json 那条覆盖规则一样：两处都能配的时候，「哪一份在生效」就成了一条必须记住的规则。

预置里同一家厂商如果国内与国际各有一套账号（智谱、Kimi、通义千问、MiniMax、硅基流动），会各出一条，国内那条名字里缀 `(CN)`。两边的密钥不通用，别拿国内的密钥去连国际站。

通义千问的预置用的是 `dashscope` 那两个通用域名。阿里云现在推荐按工作空间分配的专属域名（`{WorkspaceId}.{region}.maas.aliyuncs.com`），通用域名在 2026-09-30 之后不再加新功能，但仍可使用。专属域名里带着各人自己的工作空间 ID，做不成预置；要用的话，在这条提供商里把 Base URL 改成控制台给你的那个。

`Ollama` 那条指向本机，因此不要求密钥——凡是 `base_url` 指向 `localhost` / `127.0.0.1` 的提供商都不要求。它也刻意不带模型：装了哪些只有你自己的机器知道，先 `ollama pull`，再在这条提供商下把模型添上。它的思考参数方言是「不发送」：Ollama 给不支持思考的模型带上思考参数会直接报 400（`does not support thinking`），而支持思考的模型不带参数也会默认思考。你装的都是思考模型、又想调档位的话，把这条的方言改成「通用 reasoning_effort」。

预置里的模型 ID 是会过期的东西（各家下线旧模型相当频繁），发版时会随更新日志复核一遍；发现哪条调不通，在界面上把它改掉即可。

### 思考参数方言

OpenAI 兼容协议只统一了 messages / tools / stream 这一层，**思考相关的参数是各家的私有扩展**，互不兼容，所以要按提供商选一种「方言」。预置已经选好了，只有自定义提供商才需要自己挑：

| 方言 | 线上写法 | 接受的档位 |
|---|---|---|
| `deepseek` | `thinking:{enabled\|disabled}` + `reasoning_effort` | 五档全收 |
| `glm` | 同上 | `low` / `high` / `max` |
| `minimax` | `thinking:{adaptive\|disabled}` + `reasoning_split`；用 `max_completion_tokens` | 只有开关 |
| `qwen` | `enable_thinking` 布尔 | 只有开关 |
| `kimi` | `reasoning_effort`；不发采样参数，用 `max_completion_tokens` | `low` / `high` / `max`（`off` 发成 `low`：K3 的思考关不掉） |
| `openai` | `reasoning_effort`；不发采样参数，用 `max_completion_tokens` | 六档全收（`off` 发成 `none`） |
| `gemini` | `reasoning_effort` | `low` / `medium` / `high` |
| `xai` | `reasoning_effort` | `low` / `medium` / `high` / `xhigh` |
| `effort` | `reasoning_effort` | 五档原样发出 |
| `none` | 不发送任何思考参数 | —— |

界面上那六档（`off` / `low` / `medium` / `high` / `xhigh` / `max`）取自 Anthropic，别家没一家全收。选到对方没有的一档时**向上取整**：取不低于所选档位的最低一档，没有更高的就取最高档——选了 `medium` 的人要的是「比 low 多想一点」，给他 low 是给少了。所以在 `glm` 方言下选 `medium` 实际发出的是 `high`，在 `gemini` 下选 `max` 实际发出的是 `high`。

有一处收敛救不了：`glm-5.3` 强制思考，把档位调到「关闭」它会直接报错。预置里给这个模型钉了 `high`，别去动它。

支持两种 API 模式：

| 模式 | 说明 |
|---|---|
| `openai_compat` | OpenAI 兼容协议（DeepSeek 等） |
| `anthropic` | Anthropic Messages API。思考档位映射为 `thinking:{type:"adaptive"}` + `output_config.effort`，并要求返回思考摘要（新一代模型默认不返回思考文本）；`off` 先发 `disabled`，模型不接受时按报错改用 `between_tools`（Sonnet 5.5），或退到最低档 `low`（Fable 5.1、Opus 5.5 这类思考常开的模型：在它们上面选「关闭」仍会思考并计费，只是想得最少），学到的写法在进程内记住；当前世代 Claude 模型不接受采样参数，故不发送 `temperature`，模型条目里的该项对 Anthropic 无效 |

Anthropic 模式下多一个**提示词缓存**开关（默认开启）：开启时把请求里不变的部分（工具、system、已有历史）标为可缓存，命中的部分按约十分之一的价格计费。代价是未命中时的写入要多付约四分之一，而缓存有效期只有几分钟——如果你的对话节奏比这更慢（比如通过 IM 断断续续地聊），关掉更省。OpenAI 兼容模式没有这个开关：DeepSeek 与 OpenAI 的缓存都是服务端自动维护的，命中与否只取决于请求前缀稳不稳定（见[上下文的组织](context.md)）。

Anthropic 模式只回传**本轮**的思考块，已经结束的轮次只保留正文与工具调用。新一代模型（Fable 5.1、Opus 5.5、Sonnet 5.5）只接受「之前的内容一字没改」的思考块，而本项目的历史在轮次之间是会变的：本轮状态块只拼在当轮输入上，心跳这类一次性输入事后剔除，日界线事后补上，还有压缩。原样回传上一轮的思考块就会整个请求 400，剥掉它们是官方给出的做法之一。代价是：Opus 4.5 起、Sonnet 4.6 起的模型默认会保留之前各轮的思考，现在看不到了；Haiku 4.5 与更早的模型本来就只保留最后一轮。本轮之内若前缀也变了（预算裁剪、工具列表在轮中变化），收到这个 400 时会剥掉全部思考块自动重发一次。

