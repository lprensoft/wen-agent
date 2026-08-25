# 插件安装、生态兼容与语言选择：调研报告

[← 返回文档索引](../README.md)　·　决策记录，只有中文

> **调研日期 2026-08-25，对齐 v0.7.0 的代码现状。**
>
> 起因是「能不能像 openclaw 那样用 `plugins install` 装插件」，以及「若 Go 在这件事上有
> 意料外的限制，是否趁项目前期换语言重构」。
>
> **这是当时的判断依据，不是现状描述。** 它记录的是一次架构选择的理由——为什么这个项目
> 继续用 Go、外源插件为什么走 Agent Plugins 标准——而不是已实现功能的说明；文中「建议」
> 「打算」一类的话是当时的倾向，不构成路线图承诺。代码现状以 README、`docs/` 与 CLAUDE.md
> 为准，它们与本文冲突时以它们为准。
>
> 文中引用的外部规范发布于 2026 年 8 月初，链接与条款都可能变动，据此动手前先核对原文。
>
> 决策记录不进 `docs/` 的中英双语体系：它写给维护者与贡献者，记的是一次判断的来龙去脉，
> 不随代码更新，翻一份并长期同步的成本高于收益。使用文档仍按双语约定维护。

## 结论

**不要为这件事换语言。** Go 没有堵住任何一条实际想走的路——所担心的「便利安装被堵死」
经不起验证：行业刚刚固定下来的 Agent Plugins 1.0 标准本身就是无代码的，Go 侧配套
（官方 MCP SDK）齐备。真正被 Go 挡住的只有「第三方进程内行为插件」一项，而它对一个
装着用户全部私人数据的个人 agent 来说本就该三思。

建议的下一步是做 Agent Plugins 1.0 客户端（`wen plugins install <本地目录|git|zip>`）：
一次实现，接上六家客户端共享的插件库，且不欠任何人的接口。

## 一、关键发现：这里是三层，不是两层

原先以为「openclaw 插件」是个单一格式。实际是三层，边界划得非常清楚。

### 第 1 层：Agent Plugins 1.0 —— 厂商中立标准

2026-08-06 发布，由 OpenAI、微软、AWS、Cursor（Anysphere）、Vercel 联合推出。
首发支持的客户端有 ChatGPT、Codex CLI、Cursor、GitHub Copilot、VS Code、Kiro。

格式是一个目录：

```
plugin-root/
├── plugin.json      # 必需，闭合 schema：$schema/name/version/description/
│                    #   author/homepage/repository/license/keywords/extensions
├── skills/          # 固定位置，每个直接子目录一个 SKILL.md（Agent Skills 规范）
├── mcp.json         # 固定位置，mcpServers 映射，支持 stdio / streamable-http / sse
└── <反向域名 namespace>/   # 各客户端私有扩展目录，不认识的一律忽略
```

**这份标准里没有任何可执行代码。** 它就是「若干篇 markdown + 若干条 MCP server 声明」。
语言无关是它的设计前提，不是巧合——正因为要让 ChatGPT、VS Code、Cursor 这些用不同
语言写的宿主都能读。

### 第 2 层：openclaw 对 bundle 的处理

openclaw 能装四种 bundle：Agent Plugins（`plugin.json`）、Codex（`.codex-plugin/`）、
Claude（`.claude-plugin/`）、Cursor（`.cursor-plugin/`）。它的文档里有一句话是整份
报告的核心：

> OpenClaw does not load arbitrary bundle runtime modules in-process.

**openclaw 自己都不把 bundle 的代码加载进进程**，只做「特性映射」：skills 目录当技能
加载，stdio MCP 起子进程，settings 导成默认值。所以 bundle 这一层对宿主的要求，
wen 全都满足得了。

### 第 3 层：openclaw 原生插件

`openclaw.plugin.json` + 一个**进程内加载的 JS/TS 运行时模块**，通过 `api.on(...)`
（带优先级、合并语义、可阻断）与 `api.registerHook(...)` 挂宿主生命周期，能扩展
channels、model providers、agent harness、tools、speech、web search 等等。官方文档
明写着这些插件按**可信代码**对待（装依赖时只加了个 `--ignore-scripts`）。

这一层就是「宿主 API 才是墙」的那堵墙——但它是 openclaw 的私有生态，不是所谓的
「插件库」。

## 二、能不能兼容 openclaw 的插件库

| 层 | 能否兼容 | 代价 |
|---|---|---|
| Agent Plugins 1.0 bundle | **能，而且该做** | 读 JSON + 扫目录 + 起子进程。wen 已有的两样东西正好对上标准的两个组件 |
| Codex / Claude / Cursor bundle | 能，同上 | 只是清单位置不同，多几个分支 |
| openclaw 原生插件 | 不能，也不值得 | 等于内嵌 node + 实现 openclaw 全部宿主 API |

关键的认知修正：**不需要「兼容 openclaw」这个目标。** 实现 Agent Plugins 1.0，
一次性兼容的是 ChatGPT、Codex、Cursor、Copilot、VS Code、Kiro 加上 openclaw 的
bundle 层——同一批东西，而且对方是标准而不是某家产品，不会因为 openclaw 改版而失效。

## 三、Go 是不是堵死了便利安装

四条证据，都指向「没有」。

1. **标准本身不含代码。** 客户端要做的是：解析 `plugin.json`（闭合 schema）、扫
   `skills/`、读 `mcp.json`、起子进程并注入 `PLUGIN_ROOT` / `PLUGIN_DATA`、做路径
   包含性校验。没有一处用 Go 写比 TypeScript 难。
2. **MCP 有官方 Go SDK 且已稳定。** `modelcontextprotocol/go-sdk` v1.0.0 起承诺不再
   破坏性变更，现为 v1.5.0，与 Google 合作维护。此前「手写 JSON-RPC」的设想基于旧
   印象，直接用 SDK 即可。
3. **分发方式不在标准里，由客户端自定。** openclaw 选了 npm + ClawHub + git + 本地
   目录 + 压缩包。wen 只做「本地目录 / git / zip」三种就是合规客户端，一行 npm 都不必
   碰——npm 那条路是 openclaw 因为自己是 node 才顺手用的。
4. **「装完要重启」不是 Go 的妥协。** openclaw 文档原文：改了插件代码、启用状态、
   hook 策略之后必须重启 Gateway。一个 node 写的、插件跑在进程内的系统尚且如此。
   而 wen 的重启链路（自更新那套优雅关闭 + 原地 exec）比它还成熟。

另有一处设计判断的修正：既然一个 bundle 可能同时带多台 MCP server 和多篇技能，
**在 wen 里它应该是一张插件卡（一个开关），而不是每台 server 一张卡**——用户装的是
bundle，开关也该按 bundle 给。

## 四、换语言的账

**换来的唯一真东西**：第三方能写「行为插件」——进程内挂 wen 的生命周期钩子
（`TurnPrompt`、`OnCompact`、`DecideScope`）。wen 最值钱的插件全是这一类，这个诉求
是真实的。

**换不来的**（用 Go 也能有）：安装便利性、Agent Plugins 生态、MCP 全部能力、技能库。
这四样与语言无关。

**要付的代价**：

- 单文件 exe 没了。当前整个发布与自更新链路（六平台交叉编译、SHA256 清单、替换一个
  文件完成更新、Linux 上 execve 原地换映像）建立在「一个二进制、无运行时依赖」上，
  换 node/python 要整套重做，用户还得先装运行时。
- **信任模型显著变差。** openclaw 自己的文档就写着原生插件按可信代码对待。wen 里跑着
  用户的全部长期记忆、会话原文、IM 凭据，以及一个能执行任意命令的工具——把第三方代码
  请进这个进程，比 Go 的那点限制严重得多。进程外反而是这个产品该要的性质。
- 前期沉没成本不小：`internal/plugin` 那套契约（可见域、CompactObserver、DayReporter、
  失败转译、通道路由）是本项目真正的设计资产，重写它比重写工具代码贵。

**想要外部行为插件又不换语言的两条增量路径**：

- 把 wen 自己的钩子开成 JSON-RPC 远端方法（`SystemPrompt` / `TurnPrompt` / `OnCompact`
  变成对子进程的调用）。每轮多几次本机 round-trip，相对一次模型调用可忽略。
- 内嵌纯 Go 的脚本运行时：goja（JS）或 wazero / Extism（WASM），都能编进单文件 exe、
  不需要 cgo。WASM 那条还顺带给出 openclaw 没有的沙箱。

两条都是加法，不需要重构。

## 五、真要做的话，wen 侧的适配缺口

已有的两块直接对得上：`skills` 插件的目录扫描（`internal/plugin/builtin/skills/scan.go`
已经是「目录名即标识、必须有 description、一处坏掉不影响其余」，与 Agent Skills 规范
基本一致），以及 README「后续规划」里预留的外源 MCP 插件那一条。缺的是：

1. `plugin.json` 的闭合 schema 校验——未知字段要**报告并忽略**，不能判为无效。
2. stdio server 必须注入 `PLUGIN_ROOT` / `PLUGIN_DATA`，并在 `args` / `env` 值 / `cwd`
   里做 `${...}` 展开（一次性、非递归，`command` 不展开）。
3. 路径包含性校验：拒绝逃出插件根的符号链接 / junction，且按「最窄边界」失败——单个
   server 越界只跳过它，不整个拒绝插件。
4. 名字规整：标准的 `name` 允许连字符和点，而 wen 的 `validName` 只认下划线，且它要
   拼 `StateDir` 目录。
5. 每篇技能与每台 server 的失败都必须是**局部**的（标准明确规定），不能一处坏掉整个包
   不加载。

此外，此前那份 MCP 方案里的几个待定点仍然有效且会立刻踩到：工具名前缀化（`register`
的全局重名检查会因一个撞名拒掉整台 server）、远端描述是英文与「提示词一律中文」的
冲突、返回体量的截断与分页必须在适配层且可配、调用前确认开关、`Stoppable` 与 `Init`
可重入。

还有一条是调研当时没有、v0.7.0 才出现的：**界面与插件元数据成了中英双语**
（`internal/plugin/i18n.go`——键由插件名与字段名推导，译文在随程序发布的语言包里，
展示那一刻才翻）。外源插件的元数据来自外部，键不可能预置在语言包里，这条链路对它天然
失效。具体怎么处理见下面附录里的第 5 条。

还有两条与本项目现有约定的接口：

- **`Init` 在写锁内被调用**（`manager.go` 的 `register` 与 `SetEnabled` 都是），所以
  MCP 适配插件的 `Init` 不能做握手 I/O。install 时握手一次、把工具清单落盘，`Init`
  只读缓存——这同时让 `wen config plugins` 的离线模式（`WithoutInit`）也能列出工具。
- **存在与状态分两处且划清**：bundle 清单管「装了哪些」（只有 install/uninstall 动它），
  `plugins.state.json` 照旧管开关与配置，与内置插件一视同仁。

## 参考

- Agent Plugins 规范 v1.0.0：https://github.com/agentplugins/agent-plugins-spec/blob/main/spec/1.0.0.md
- agent-plugins.org：https://agent-plugins.org/
- Agent Skills 规范：https://agentskills.io/specification
- OpenClaw · Plugin bundles：https://docs.openclaw.ai/plugins/bundles
- OpenClaw · plugins CLI：https://docs.openclaw.ai/cli/plugins
- OpenClaw · 原生插件：https://docs.openclaw.ai/tools/plugin
- modelcontextprotocol/go-sdk v1.0.0：https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.0.0
- Vercel：Introducing Agent Plugins：https://vercel.com/blog/introducing-agent-plugins
- AWS Supports Agent Plugins：https://aws.amazon.com/blogs/opensource/aws-supports-agent-plugins-an-open-standard-for-portable-agent-extensions/

## 附：实现 Agent Plugins 1.0 客户端的工作量拆解

总量约 1800–2200 行（含测试），新增 3 个包 + 1 条 CLI 命令 + 一处启动接线。
拆成四段，**每一段都能独立发布、单独就有用**。这个拆法的依据是标准里的一句话：

> Clients need not support all component types but must support at least one.

**只做 skills 就已经是合规客户端**，不碰 MCP、不引 SDK、不起子进程。

### M1：skills-only 客户端（约 400 行 + 200 行测试，零新依赖）

| 文件 | 内容 | 估行 |
|---|---|---|
| `internal/agentplugin/manifest.go` | `plugin.json` 解析 + 闭合 schema 校验（10 个字段，手写即可，不必引 JSON Schema 库） | ~120 |
| `internal/agentplugin/path.go` | 路径包含性校验：`EvalSymlinks` + 前缀比较，按「最窄边界」失败 | ~80 |
| `internal/agentplugin/load.go` | 目录扫描、组件发现、局部失败收集 | ~120 |
| `internal/agentplugin/install.go` | 本地目录 / zip 安装、卸载，落到 `<配置目录>/bundles/<name>/` | ~150 |
| `cmd/wen/plugins.go` | `wen plugins list / install / uninstall / info` | ~180 |

技能发现直接复用 `internal/plugin/builtin/skills/scan.go`——它现在就是「目录名即标识、
必须有 description、一处坏掉不影响其余」，与 Agent Skills 规范基本重合，抽成共享函数即可。
做完这一段就能装社区的技能包，是投入产出比最高的一步。

### M2：MCP stdio（约 600–700 行 + 300 行测试）

| 文件 | 内容 | 估行 |
|---|---|---|
| `internal/agentplugin/mcp.go` | `mcp.json` 解析、三种 transport 的字段校验、`${PLUGIN_ROOT}` / `${PLUGIN_DATA}` 展开 | ~180 |
| `internal/agentplugin/client.go` | 官方 go-sdk 接入、按需拉起子进程、超时、`Stop` | ~200 |
| `internal/agentplugin/adapter.go` | 一个 bundle → 一个 `plugin.Plugin`：工具名前缀化、返回体截断与分页、`Configurable`、`Stoppable` | ~250 |
| `cmd/wen/main.go` | `buildPlugins` 扫已装 bundle → `RegisterExternal`、名字规整 | ~50 |

这一段最容易被低估的是**返回体塑形**与**工具名前缀化**（`register` 的全局重名检查会因
一个撞名拒掉整台 server）。

### M3：Streamable HTTP（约 150 行）

SDK 已有 transport，主要是配置字段与 URL 校验（非回环必须 HTTPS、不许带 userinfo 与 fragment）。

### M4：界面入口 + 其它 bundle 格式（约 200 行）

设置页的安装入口；Codex / Claude / Cursor 三种清单位置各约 30 行分支。

### 已有的可复用资产

1. 技能扫描（`skills/scan.go` 几乎就是 Agent Skills 的实现）；
2. CLI 命令面（`cmd/wen/cli.go` 的命令表加一条就有帮助文本、参数校验、未知命令提示）；
3. 在线/离线双模（`cmd/wen/backend.go`，install 本质是动文件，离线也能做）；
4. 外源插件的展示（`RegisterExternal` 与「外源」标签、卡片、开关、`StateDir` 全都现成；
   `Status` 结构基本够用，只需给出 v0.7.0 新增的 `CategoryKey`，界面按它分组）；
5. 重启链路（自更新那套优雅关闭 + 原地 exec，装完一步到位）。

新依赖只有一个：`modelcontextprotocol/go-sdk`，且 M2 才需要。

### 需要先拍板的四件事

1. **bundle 的技能往哪儿汇。** 倾向进 `skills` 插件的同一份清单、共用 `read_skill`，而不是
   每个 bundle 自带一套工具（N 个 bundle 就是 N 个同名工具）。做法是加一个进程级的技能根
   注册表（`internal/skillroots`，与 `cue`、`imbot` 同级的共享基础设施，核心不知道它），
   bundle 启用时登记、禁用时摘掉，skills 一并扫。约 40 行，但这是个要点头的架构决定。
2. **git 安装做不做。** 第一版建议不做：本地目录 + zip 已覆盖绝大多数场景，git 要么 shell
   出去（依赖用户装了 git）要么引一个纯 Go 的 git 库（重）。用户自己 clone 完再
   `install <目录>` 完全够用。
3. **registry / marketplace 做不做。** 建议不做。标准明确不规定分发方式，openclaw 那套
   ClawHub + npm 是它自己的选择，跟不跟没有兼容性影响。
4. **M1 与 M2 之间要不要发一版。** 建议要——M1 是纯读文件、零新依赖、无子进程，风险极低；
   M2 才开始牵扯进程管理、确认开关、上下文体量这些真正需要打磨的东西。
5. **外源插件的元数据在双语界面里怎么呈现。** 分两类，别混为一谈：
   - **来自 bundle 的文字**（`plugin.json` 的 description、远端工具的描述）是外部数据，
     语言包里不会有它的键，只能原样透传。方向与内置插件正好相反——内置是中文原文译成
     英文，外源多半是英文原文，于是中文界面上就会看到英文。这可以接受（它本来就是外部
     数据），但要让人看得出这是来源如此，而不是以为某个插件漏翻了。
   - **我们给外源插件加的配置项**（调用前确认、截断阈值、描述覆写）是本项目的文案，该进
     语言包。但键按 `plugin.<插件名>.field.<字段>.label` 推导会与 bundle 名绑死，而 bundle
     名要装了才知道——所以这批字段需要一个**与 bundle 名无关的共享键前缀**，否则每装一个
     bundle 就多出一组永远翻不了的标签。这一条会动到 `internal/plugin/i18n.go` 的键推导，
     动手前先定。
