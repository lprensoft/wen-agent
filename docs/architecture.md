# 项目结构与插件开发

[← 返回 README](../README.md)　·　中文　·　[English](en/architecture.md)

## 项目结构

```
cmd/wen/                 入口（注册内置插件、装配各层）
internal/config/         配置加载（YAML + ${VAR} 环境变量替换）
internal/llm/            Provider 接口 + OpenAI 兼容 / Anthropic 实现
internal/modelcfg/       模型与提供商配置（models.json 覆盖层，热切换）
internal/agent/          Agent 循环（工具调用 / 思考 / 压缩 / 上下文预算）
internal/plugin/         插件协议（Plugin / Tool / Configurable / 观察者 / 可见域 / 依赖）+ Manager（开关与聚合）
internal/plugin/builtin/ 内置系统插件（注册顺序即提示词拼接顺序）：
                         readfile / execcmd / webfetch /
                         roleplay / dualpersona / scene / weather / belongings / bodysense / health / mood / people / agenda / relationship / unspoken / presence / stylewatch /
                         memory / sessionsearch / skills /
                         heartbeat / scheduler /
                         qqbot / wechatbot / larkbot（飞书 + Lark）/ telegrambot /
                         selfupdate
internal/imbot/          消息通道的公共骨架（分发 / 命令 / 会话绑定 / 确认代理 / 通道路由）
internal/cue/            「开口理由」公告板：产生方按事件投递，心跳开口前取走
internal/availability/   「忙碌状态」公告板：日程在活动进行中写，心跳与消息通道读
internal/stylecheck/     「助手腔」检测规则与字数 / 句数 / 演绎占比度量，纯函数，style_watch 与 wen eval 共用
internal/mdtext/         markdown 分段与转纯文本，各通道的格式降级共用
internal/textclip/       插件注入文本的预算截断，各插件共用
internal/statustext/     状态输出的措辞，各通道的 /status 共用
internal/runlock/        运行实例登记（wen.lock），wen config 据此判定在线 / 离线
internal/updater/        自更新：查发布、下载校验、替换可执行文件、重启（三平台各自的做法）
internal/session/        JSONL 会话存储
internal/server/         HTTP API + SSE + 内嵌 Web UI
internal/version/        版本号的唯一来源（界面、/status、启动日志与 exe 属性共用）
tools/                   构建期生成器：genicon（favicon）、genwinres（Windows 版本资源）
```

## 编写插件

实现 `internal/plugin` 的 `Plugin` 接口（`Name` / `Description` / `Init` / `SystemPrompt` / `Tools`），在 `cmd/wen/main.go` 的 `buildPlugins` 注册即可。约定：提示词与描述用中文写，再经 `prompt.T(lang, "键", 中文)` 取用（见[配置与模型](configuration.md#语言)；中文不进语言包，它既是源语言也是兜底），只写功能、不含身份信息；`SystemPrompt()` 返回空串表示不注入；插件名限小写字母、数字与下划线（它会被用来拼持久化目录）。

可选接口（不实现则零成本）：

- `Configurable`（`ConfigFields()`）——声明可配置项，设置页据此生成表单并持久化。字段类型有 `int` / `bool` / `string` / `select` / `text`（多行，渲染成 textarea）。
- `CompactObserver`（`OnCompact()`）——在会话历史被摘要替换**之前**收到通知，可借此归档或提炼；返回的注记会追加到摘要末尾，从而只落进该会话的历史。事件广播给所有订阅者（`memory` 与 `session_search` 都订阅了它，各做各的事），返回错误只记日志，不阻断压缩。历史带可见域标签时按标签分组，每组一次事件。
- `ScopeDecider`（`DecideScope()`）与 `TurnPrompter`（`TurnPrompt()`）——见下方「可见域」。
- 操作确认：用 `plugin.ConfirmerFrom(ctx)` 取确认通道，在执行不可逆操作前问一次。第二个返回值为 false 表示当前没有可交互的用户，**不要当作已获同意**；返回 error 同理，拿不到答复不等于得到许可。
- `Dependent`（`Requires()`）——声明必须同时启用的插件。依赖未满足时拒绝启用（开关在界面上置灰），被依赖的插件也无法在依赖方仍启用时关闭。
- `Conflicting`（`Conflicts()`）——声明能力相抵的插件。只告警不阻止。
- `Stoppable`（`Stop()`）——停掉自己起的后台活动。禁用、以新配置重新 `Init`、进程退出三处会调用；只做取消与有界等待，不得等整轮对话跑完。起 goroutine 的插件必须实现它，并保证 `Init` 可重入。
- `TurnObserver`（`OnTurnEnd()`）——观察每轮对话的结束。在收尾的同步路径上广播，实现必须快速返回，耗时工作自行开 goroutine。
- `StatusReporter`（`StatusLines()`）——向状态命令贡献一行运行状况。与 `SystemPrompt` 同契约（廉价、无副作用）。
- `DayReporter`（`DayFacts(date)`）——为某一天补一句客观事实（天气插件据此答那天的天气）。谁要用这份汇总，经 `InitContext.DayFacts` 取——按日收束的日记就是这么在正文前加上抬头的。同样是廉价、无副作用的契约，答不上来返回 nil，别编。
- `Actionable`（`Actions()` / `StartAction()` / `ActionState()`）——声明可在设置页触发的流程（如扫码绑定），状态含说明文字与一张可选 PNG。`StartAction` 应立即返回，长流程放后台并自带超时。
- `Categorized`（`Category()`）——声明功能分组（基础工具 / 记忆与检索 / 角色演绎 / 后台任务 / 消息通道 / 程序维护），设置页据此分节展示。只影响展示，不影响注册顺序。
- `FailureTranslator`（`TranslateFailure()`）——轮次失败时把失败转成一句正常回复的机会（`roleplay` 据此把内容拦截演成角色的走神）。单所有者、按逆注册序征询，只在真人在场的轮次尝试；转译文本以正常助手消息落盘，原始错误转入会话注记；配置类错误（`llm.IsConfigError`）必须放行原样报出。
- `NoticeObserver`（`OnNotice()`）——会话注记落盘并送达 Web 界面之后收到广播（IM 通道据此把后台工作的说明推给绑定用户）。事件携带的是已截断的落盘文本。

插件之间还有三样共享的基础设施，核心不知道它们：`internal/imbot`（消息通道骨架，见下）、`internal/cue`（「开口理由」公告板——产生方按**事件**投递而不是按状态投递，同 (Source, Key) 幂等覆盖，必须带过期时刻；心跳开口前取走全部未过期的理由。只存内存，检测状态的权威在产生方自己的持久化里）与 `internal/availability`（「忙碌状态」公告板——存状态不存事件，每个写入方只有一条，查询不清、靠到期时刻自然过期；日程在活动进行中写，心跳与消息通道据此把下一拍压后或延后处理入站消息。同样只存内存，重启后由日程从当天的表重建）。

`InitContext` 提供这些运行环境：`Workdir`（工作目录）、`StateDir`（该插件专属的持久化目录 `<配置目录>/plugins/<插件名>/`，可能不存在需自行创建）、`Sessions`（会话的只读窄查询：最近活跃的会话、某会话是否还在）、`SessionDir`（会话目录，只给需要读会话正文的检索与归档类插件）、`Complete`（用当前模型做一次一问一答的辅助调用，不带工具、不写会话）、`RunTurn` / `NewSession` / `Compact`（以插件身份跑一轮完整对话、新建会话、压缩历史）、`Status`（模型配置与会话用量快照）、`DayFacts`（问某一天有什么可说的，汇总各插件的回答）、`Notice`（往会话里留一行只给人看的说明）、`Lang`（这个进程的语言，插件在 `Init` 里存下，此后提示词、工具描述、状态行与日志都按它取）。除 `Workdir` 外为空/nil 均表示当前不可用，插件应据此拒绝启用或降级，不要退化到写进程当前目录。`Complete` 与 `RunTurn` 每次调用都产生真实的模型开销，只放在低频路径上。

`RunTurn` 与 `Notice` 由 Manager 包一层自动注入发起方标记，插件无法伪装成前台；`RunTurn` 在会话忙时立即返回 `ErrSessionBusy` 而不排队——后台任务堆在锁上，只会在解锁瞬间连环轰炸同一个会话。`Notice` 写出的内容落盘、在界面实时展示，但永不进入模型上下文，也不进压缩摘要：后台工作在轮次收尾之后才跑完，那时事件流已经关闭，结果否则只能进日志。

再写一条消息通道的话，通道无关的部分（入站分发与去重、命令集、按用户串行、会话绑定、确认代理、通道路由）在 `internal/imbot` 里，插件只需实现 `imbot.Sender`（把一段文本发给某个用户）并把入站消息归一化成 `imbot.Message`。

### 可见域

同一会话内的消息可以分区，使一部分内容对某些轮次不可见。标签的语义完全由插件定义，核心只做三件事：给消息打标签、按标签过滤历史、按标签分组压缩——它不知道「人格」这回事。**空标签始终对所有域可见**，因此升级前无标签的历史继续可用，「共享」也不需要任何插件参与就成立。

两阶段：`ScopeDecider.DecideScope` 先定域（单所有者机制，按注册顺序第一个返回非零 `Scope` 的插件胜出），`TurnPrompter.TurnPrompt` 再按已定的域注入一次性提示词。之所以分开，是因为 `memory` 要按可读域过滤记忆索引，而域由别的插件裁决，单阶段广播里两者无序。可见域随后经 `context` 传给工具执行，用 `plugin.ScopeFrom(ctx)` 取。

`SystemPrompt()` 的签名不变：它的契约是廉价、无副作用、随时可调（插件列表接口会对**禁用**的插件也调用它），需要会话上下文的内容一律走 `TurnPrompt`。

要把数据按可见域分开存放，用 `plugin.DomainDir` 与 `plugin.ReadDomains`：空标签用基准目录本身，其余域用同级的 `<base>-<tag>`。标签会被拼进目录，故按插件名的字符集校验，不合规的裁决整条作废。

可见域还能绑通道：插件安装 `imbot.SetRouter` 之后，各通道就按「这个会话的回复该发往哪条通道」投递，与这一轮从哪条通道进来无关（`dual_persona` 用它让表里两面各占一条 IM）。这部分在 `internal/imbot` 里，核心不参与，`imbot` 也只认通道名。

**可见域是上下文隔离，不是沙箱**——任何提供通用文件或命令通道的插件都能绕过它。

