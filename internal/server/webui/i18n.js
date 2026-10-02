"use strict";

// Web UI 的界面文案（中文 / 英文）。
//
// 这里只管**界面外壳**：服务端下发的文字（插件名称与说明、配置项标签、状态行、
// 会话注记）仍旧由服务端给出，目前只有中文，界面原样展示。
//
// 为什么在首屏之前同步加载：文案是填进 DOM 的，晚一步就会先闪一屏旧语言再跳成新的。
// index.html 的 <head> 里用普通 <script src> 引入（解析器阻塞，本地 embed 没有网络成本），
// 挂了键的元素在 HTML 里是**空的**，由解析过程中的观察器就地填好——留着原文的话，
// 解析器会在填好之后再把原文的文本节点追加进去，两份文字叠在一起。
//
// 三态与主题控件同构：auto / zh / en，存 localStorage["wen-lang"]，默认 auto。
// 只存浏览器、不上服务端：同一个服务可能一台电脑一台手机各用一种语言，而服务端配置
// 是共享的。三态也不能压成两态——只存 zh/en 的话，「我从没选过」与「我选了中文」
// 是同一个值，之后改了浏览器语言界面就跟不动了。
//
// 新增界面文案时两种语言一起补：缺键时 t() 回落中文并在控制台留一行，不会静默变空白。

(function () {
  const DICT = {
    zh: {
      // ---- 通用 ----
      "common.save": "保存",
      "common.cancel": "取消",
      "common.close": "关闭",
      "common.back": "返回",
      "common.loading": "加载中…",
      "common.resetDefaults": "恢复默认",
      "common.increase": "增加",
      "common.decrease": "减少",
      "common.unknownError": "未知错误",

      // ---- 侧栏与会话 ----
      "session.new": "＋ 新会话",
      "session.newTitle": "新建会话",
      "session.untitled": "（新会话）",
      "session.delete": "删除会话",
      "session.deleteConfirm": "删除该会话？",
      "settings.open": "设置",
      "theme.title": "界面风格",
      "theme.system": "跟随系统",
      "theme.light": "浅色",
      "theme.dark": "深色",

      // ---- 聊天区 ----
      "chat.empty": "新建或选择一个会话开始对话",
      "chat.placeholder": "输入消息，Enter 发送，Shift+Enter 换行",
      "chat.send": "发送",
      "chat.stop": "停止生成",
      "chat.filterTitle": "选择聊天区显示哪些内容",
      "chat.filterChip": "显示内容",
      "chat.filterHeading": "聊天区显示以下内容",
      "chat.filter.prompt": "发给模型的提示词",
      "chat.filter.promptNote": "调试用",
      "chat.filter.tools": "工具调用",
      "chat.filter.thinking": "思考过程",
      "chat.filter.heartbeat": "心跳唤醒",
      "chat.modelTitle": "切换当前使用的模型",
      "chat.modelUnavailable": "模型不可用",
      "chat.modelNone": "未选择模型",
      "chat.modelChipTitle": "{name}（点击切换）",
      "chat.modelMenuLoading": "模型清单加载中…",
      "chat.modelMenuTitle": "切换模型",
      "chat.modelNoModels": "无模型",
      "chat.modelNoKey": "未配置密钥",
      "chat.modelSwitching": "切换中…",
      "chat.modelSwitchFailed": "切换模型失败：{msg}",

      // ---- 消息区里的各种块 ----
      "block.summary": "📦 历史摘要（已压缩）",
      "block.thinking": "🧠 思考过程",
      "block.tool": "🔧 调用工具",
      "block.toolArgs": "参数: ",
      "block.toolResult": "结果: ",
      "block.toolRunning": "执行中…",
      "block.prompt": "📤 发给模型的提示词 ",
      "block.promptSize": "（{n} 条消息，{size}）",
      "block.heartbeat": "💓 心跳唤醒",
      "block.backgroundWake": "⏱ 后台唤醒（{origin}）",
      "block.backgroundWakeSystem": "系统",
      "block.confirmTitle": "需要确认",
      "block.confirmDeny": "拒绝",
      "block.confirmAllow": "允许执行",
      "block.confirmAllowed": "已允许",
      "block.confirmDenied": "已拒绝",
      "block.confirmExpired": "已超时，按拒绝处理",
      "block.confirmSubmitFailed": "提交失败：{msg}",

      "size.chars": "{n} 字符",
      "size.kchars": "{n}k 字符",
      "size.mchars": "{n}M 字符",

      // ---- 一轮对话的过程与收尾 ----
      "chat.compacting": "📦 上下文接近上限，正在自动压缩会话…",
      "chat.compactFailed": "自动压缩失败：{msg}",
      "chat.error": "出错了：{msg}",
      "chat.noText": "（本轮没有文本回复）",
      "chat.requestFailed": "请求失败：{msg}",
      "chat.stopped": "⏹ 已停止生成",
      "chat.autoCompacted": "✅ 会话已自动压缩",

      // ---- 斜杠命令 ----
      "cmd.statusDesc": "显示 Agent 状态：模型、思考深度、上下文用量、会话 ID、插件状态",
      "cmd.compactDesc": "压缩当前会话为摘要",
      "cmd.unknown": "⚠️ 未知命令：{cmd}\n可用命令：/status、/compact",

      "status.head": "📊 Agent 状态",
      "status.model": "模型：{provider} / {model} · 思考深度 {thinking}",
      "status.session": "当前会话：{count} 条消息，{used} / {total} tokens（占用 {pct}%）",
      "status.sessionApprox": "当前会话：{count} 条消息，约 {used} / {total} tokens（占用 {pct}%）",
      "status.cache": "提示词缓存：命中 {hit} tokens",
      "status.cacheWrite": "提示词缓存：命中 {hit} / 写入 {write} tokens",
      "status.cacheShare": "（占本轮输入 {pct}%）",
      "status.sessionId": "会话 ID：{id}",
      "status.noSession": "当前会话：无（上下文窗口 {total} tokens）",
      "status.failed": "获取状态失败：{msg}",

      "compact.noSession": "⚠️ 没有活动会话，无法压缩",
      "compact.running": "📦 正在压缩会话…",
      "compact.failed": "压缩失败：{msg}",
      "compact.done": "✅ 压缩完成",

      // ---- 设置页外壳 ----
      "settings.title": "设置",
      "settings.nav.general": "通用",
      "settings.nav.plugins": "插件",
      "settings.nav.access": "访问控制",
      "settings.nav.models": "模型",

      "settings.general.title": "通用",
      "settings.general.desc": "界面外观设定，只影响本机的浏览器显示，不写入服务端配置。",
      "settings.language.label": "界面语言",
      "settings.language.desc": "只影响本机的浏览器显示，不写入服务端配置。",
      "settings.language.auto": "跟随浏览器",
      "settings.language.zh": "中文",
      "settings.language.en": "English",
      "settings.chatWidth.label": "聊天栏宽度",
      "settings.chatWidth.desc": "消息与输入框所占的栏宽，窗口不够宽时自动收窄。当前 {width}。",
      "settings.chatWidth.narrow": "窄",
      "settings.chatWidth.medium": "中",
      "settings.chatWidth.wide": "宽",

      // ---- 插件 ----
      "settings.plugins.title": "插件",
      "settings.plugins.desc": "开关立即生效并持久化，作用于下一次对话请求。",
      "settings.plugins.loadFailed": "加载插件列表失败：{msg}",
      "settings.plugins.configure": "配置",
      "settings.plugins.source.builtin": "内置",
      "settings.plugins.source.external": "外源",
      "settings.plugins.hasPrompt": "注入提示词",
      "settings.plugins.blocked": "需先启用 {names}",
      "settings.plugins.blockedTitle": "该插件依赖 {names}，需要先启用后才能打开",
      "settings.plugins.requires": "依赖 {names}",
      "settings.plugins.conflicts": "与 {names} 能力相抵",
      "settings.plugins.tools": "工具：{names}",
      "settings.plugins.toggleFailed": "切换插件失败：{msg}",
      "settings.plugins.configModalTitle": "插件配置",
      "settings.plugins.configTitle": "{name} · 配置",
      "settings.plugins.saveFailed": "保存失败：{msg}",
      "settings.plugins.range": "取值范围 {min} ~ {max}。",
      "settings.plugins.min": "不小于 {min}。",
      "settings.plugins.max": "不大于 {max}。",
      "settings.plugins.actionModalTitle": "插件操作",
      "settings.plugins.actionStarting": "正在开始…",
      "settings.plugins.actionStartFailed": "操作启动失败：{msg}",
      "settings.plugins.actionExpired": "登录已失效（服务可能已重启），请刷新页面重新登录。",
      "settings.plugins.actionPollFailed": "查询进展失败：{msg}",
      "settings.plugins.actionRetrying": "（与服务的连接中断，正在重试…）",

      // ---- 访问控制 ----
      "settings.access.title": "访问控制",
      "settings.access.desc": "来自本机的访问免登录，其它来源需要口令。口令存于 <配置目录>/auth.json，不写入 config.yaml。",
      "access.current": "当前口令",
      "access.new": "新口令",
      "access.newDesc": "至少 8 位。留空并保存表示清除口令（仅在只监听本机时允许）。",
      "access.confirm": "确认新口令",
      "access.logout": "退出登录",
      "access.envManaged": "口令由环境变量 WEN_AUTH_PASSWORD 提供，此处无法修改。",
      "access.noPassword": "尚未设置口令。服务当前只监听本机，配置了对外监听也会被降级——设置口令并重启后才会真正对外提供服务。",
      "access.allLogin": "已设置口令，所有来源（包括本机）都需要登录。",
      "access.loopbackFree": "已设置口令。本机访问免登录，其它来源需要登录。",
      "access.localOnly": "已设置口令，但当前只监听本机，外部访问不到。",
      "access.stateFailed": "读取访问控制状态失败：{msg}",
      "access.mismatch": "两次输入的新口令不一致",
      "access.saveFailed": "保存失败",
      "access.saveFailedMsg": "保存失败：{msg}",

      // ---- 模型配置 ----
      "settings.models.title": "模型",
      "settings.models.desc": "改动写入 <配置目录>/models.json，保存后下一次对话请求即生效。",
      "models.addProvider": "＋ 新增提供商",
      "models.loadFailed": "加载模型配置失败：{msg}",
      "models.current": "当前使用：",
      "models.currentNone": "未选择",
      "models.fromConfig": "来自配置文件",
      "models.editProvider": "编辑提供商",
      "models.deleteProvider": "删除提供商",
      "models.metaWithKey": "{url}  ·  API Key {masked}",
      "models.metaNoKey": "{url}  ·  未配置 API Key",
      "models.empty": "暂无模型",
      "models.addModel": "＋ 添加模型",
      "models.inUse": "当前使用中",
      "models.switchTo": "点击切换到该模型",
      "models.editModel": "编辑模型",
      "models.deleteModel": "删除模型",
      "models.switchFailed": "切换模型失败：{msg}",
      "models.deleteProviderConfirm": "确定删除提供商「{name}」？",
      "models.deleteModelConfirm": "确定删除模型「{id}」？",
      "models.deleteFailed": "删除失败：{msg}",

      "preset.modalTitle": "选择提供商",
      "preset.hint": "挑一家即可，地址与常用模型都已填好，你只要补上 API Key。",
      "preset.custom": "自定义",
      "preset.customDesc": "上面没有你要的那家，手工填写地址与参数。",
      "preset.getKey": "去 {name} 拿 API Key",
      "preset.noKeyNeeded": "本机服务，不需要密钥",

      "provider.modalTitle": "提供商",
      "provider.edit": "编辑提供商 · {name}",
      "provider.add": "新增提供商",
      "provider.name": "名称",
      "provider.nameDesc": "列表中显示的名字，需唯一。",
      "provider.type": "API 模式",
      "provider.typeDesc": "Anthropic 模式使用 Messages API；OpenAI 兼容适用于 DeepSeek 等服务。",
      "provider.dialect": "思考参数方言",
      "provider.dialectDesc": "各家 OpenAI 兼容接口的思考/推理参数写法不同：DeepSeek 用 thinking+reasoning_effort，MiniMax 用 adaptive+reasoning_split，Qwen 用 enable_thinking。选错会被对方 API 以 400 拒绝或思考内容混入正文。",
      "provider.baseUrl": "Base URL",
      "provider.baseUrlDesc": "服务地址，需以 http:// 或 https:// 开头。",
      "provider.apiKey": "API Key",
      "provider.apiKeyDesc": "保存在本机 models.json 中，不会提交到仓库。",
      "provider.apiKeyPlaceholder": "留空表示不修改（当前 {masked}）",
      "provider.cache": "提示词缓存",
      "provider.cacheDesc": "开启后把不变的部分（工具、system、已有历史）标为可缓存，命中时这部分按约十分之一的价格计费。未命中的写入要多付约四分之一，因此若对话间隔常常超过几分钟（缓存有效期），关掉更省。",
      "provider.test": "测试连接",
      "provider.testNeedModel": "请先为该提供商添加模型后再测试",
      "provider.testing": "正在测试…",
      "provider.testOk": "连接正常（模型 {model}）",
      "provider.saveFailed": "保存失败：{msg}",

      "model.modalTitle": "模型",
      "model.edit": "编辑模型 · {id}",
      "model.add": "添加模型 · {provider}",
      "model.id": "模型 ID",
      "model.idDesc": "传给 API 的模型 id，例如 deepseek-flash。",
      "model.name": "显示名",
      "model.nameDesc": "列表中显示的名字，留空则显示模型 ID。",
      "model.context": "上下文窗口",
      "model.contextDesc": "token 数，用于裁剪与自动压缩阈值。留空则使用 config.yaml 的全局值。",
      "model.maxTokens": "最大输出 tokens",
      "model.maxTokensDesc": "单次回复的输出上限。留空则使用全局值。",
      "model.thinking": "思考模式",
      "model.thinkingDescAnthropic": "Anthropic 模式下 off 之外的档位映射为 adaptive + effort。",
      "model.thinkingDesc": "off 关闭思考，其余为思考强度。",
      "model.temperature": "temperature",
      "model.temperatureDescAnthropic": "Anthropic 模式忽略此项：当前世代 Claude 模型不接受采样参数。",
      "model.temperatureDesc": "0 ~ 2，思考开启时不生效。留空则使用全局值。",
      "model.followGlobal": "跟随全局（{value}）",
      "model.saveFailed": "保存失败：{msg}",
    },

    en: {
      // ---- 通用 ----
      "common.save": "Save",
      "common.cancel": "Cancel",
      "common.close": "Close",
      "common.back": "Back",
      "common.loading": "Loading…",
      "common.resetDefaults": "Restore defaults",
      "common.increase": "Increase",
      "common.decrease": "Decrease",
      "common.unknownError": "unknown error",

      // ---- 侧栏与会话 ----
      "session.new": "＋ New chat",
      "session.newTitle": "Start a new chat",
      "session.untitled": "(New chat)",
      "session.delete": "Delete chat",
      "session.deleteConfirm": "Delete this chat?",
      "settings.open": "Settings",
      "theme.title": "Appearance",
      "theme.system": "Match system",
      "theme.light": "Light",
      "theme.dark": "Dark",

      // ---- 聊天区 ----
      "chat.empty": "Start a new chat, or pick one from the list",
      "chat.placeholder": "Type a message. Enter sends, Shift+Enter adds a line",
      "chat.send": "Send",
      "chat.stop": "Stop generating",
      "chat.filterTitle": "Choose what the chat area shows",
      "chat.filterChip": "Show",
      "chat.filterHeading": "Show these in the chat area",
      "chat.filter.prompt": "Prompt sent to the model",
      "chat.filter.promptNote": "for debugging",
      "chat.filter.tools": "Tool calls",
      "chat.filter.thinking": "Thinking",
      "chat.filter.heartbeat": "Heartbeat wake-ups",
      "chat.modelTitle": "Switch the model in use",
      "chat.modelUnavailable": "Models unavailable",
      "chat.modelNone": "No model selected",
      "chat.modelChipTitle": "{name} (click to switch)",
      "chat.modelMenuLoading": "Loading models…",
      "chat.modelMenuTitle": "Switch model",
      "chat.modelNoModels": "no models",
      "chat.modelNoKey": "no API key",
      "chat.modelSwitching": "Switching…",
      "chat.modelSwitchFailed": "Couldn't switch model: {msg}",

      // ---- 消息区里的各种块 ----
      "block.summary": "📦 Earlier conversation (compacted)",
      "block.thinking": "🧠 Thinking",
      "block.tool": "🔧 Tool call",
      "block.toolArgs": "Arguments: ",
      "block.toolResult": "Result: ",
      "block.toolRunning": "Running…",
      "block.prompt": "📤 Prompt sent to the model ",
      "block.promptSize": function (v) {
        return "(" + (v.n === 1 ? "1 message" : v.n + " messages") + ", " + v.size + ")";
      },
      "block.heartbeat": "💓 Heartbeat wake-up",
      "block.backgroundWake": "⏱ Background wake-up ({origin})",
      "block.backgroundWakeSystem": "system",
      "block.confirmTitle": "Confirmation needed",
      "block.confirmDeny": "Deny",
      "block.confirmAllow": "Allow",
      "block.confirmAllowed": "Allowed",
      "block.confirmDenied": "Denied",
      "block.confirmExpired": "Timed out, treated as denied",
      "block.confirmSubmitFailed": "Couldn't submit: {msg}",

      "size.chars": "{n} chars",
      "size.kchars": "{n}k chars",
      "size.mchars": "{n}M chars",

      // ---- 一轮对话的过程与收尾 ----
      "chat.compacting": "📦 The context is near its limit, compacting the conversation…",
      "chat.compactFailed": "Automatic compaction failed: {msg}",
      "chat.error": "Something went wrong: {msg}",
      "chat.noText": "(no text in this reply)",
      "chat.requestFailed": "Request failed: {msg}",
      "chat.stopped": "⏹ Generation stopped",
      "chat.autoCompacted": "✅ The conversation was compacted",

      // ---- 斜杠命令 ----
      "cmd.statusDesc": "Show agent status: model, thinking depth, context usage, chat ID, plugin status",
      "cmd.compactDesc": "Compact this conversation into a summary",
      "cmd.unknown": "⚠️ Unknown command: {cmd}\nAvailable commands: /status, /compact",

      "status.head": "📊 Agent status",
      "status.model": "Model: {provider} / {model} · thinking {thinking}",
      "status.session": function (v) {
        return "This chat: " + (v.count === 1 ? "1 message" : v.count + " messages") +
          ", " + v.used + " / " + v.total + " tokens (" + v.pct + "% of the window)";
      },
      "status.sessionApprox": function (v) {
        return "This chat: " + (v.count === 1 ? "1 message" : v.count + " messages") +
          ", about " + v.used + " / " + v.total + " tokens (" + v.pct + "% of the window)";
      },
      "status.cache": "Prompt cache: {hit} tokens read",
      "status.cacheWrite": "Prompt cache: {hit} tokens read, {write} written",
      "status.cacheShare": " ({pct}% of this request's input)",
      "status.sessionId": "Chat ID: {id}",
      "status.noSession": "No active chat (context window {total} tokens)",
      "status.failed": "Couldn't read the status: {msg}",

      "compact.noSession": "⚠️ No active chat to compact",
      "compact.running": "📦 Compacting the conversation…",
      "compact.failed": "Compaction failed: {msg}",
      "compact.done": "✅ Compaction finished",

      // ---- 设置页外壳 ----
      "settings.title": "Settings",
      "settings.nav.general": "General",
      "settings.nav.plugins": "Plugins",
      "settings.nav.access": "Access",
      "settings.nav.models": "Models",

      "settings.general.title": "General",
      "settings.general.desc": "Appearance settings. They apply to this browser only and are never written to the server config.",
      "settings.language.label": "Language",
      "settings.language.desc": "Applies to this browser only; nothing is written to the server.",
      "settings.language.auto": "Follow browser",
      "settings.language.zh": "中文",
      "settings.language.en": "English",
      "settings.chatWidth.label": "Chat column width",
      "settings.chatWidth.desc": "How wide the messages and the composer are. Narrows on its own when the window is too small. Currently {width}.",
      "settings.chatWidth.narrow": "Narrow",
      "settings.chatWidth.medium": "Medium",
      "settings.chatWidth.wide": "Wide",

      // ---- 插件 ----
      "settings.plugins.title": "Plugins",
      "settings.plugins.desc": "Switches take effect at once and are remembered; they apply from the next request on.",
      "settings.plugins.loadFailed": "Couldn't load the plugin list: {msg}",
      "settings.plugins.configure": "Configure",
      "settings.plugins.source.builtin": "Built-in",
      "settings.plugins.source.external": "External",
      "settings.plugins.hasPrompt": "Adds to the prompt",
      "settings.plugins.blocked": "Enable {names} first",
      "settings.plugins.blockedTitle": "This plugin depends on {names}. Enable those before turning it on.",
      "settings.plugins.requires": "Needs {names}",
      "settings.plugins.conflicts": "Overlaps with {names}",
      "settings.plugins.tools": "Tools: {names}",
      "settings.plugins.toggleFailed": "Couldn't switch the plugin: {msg}",
      "settings.plugins.configModalTitle": "Plugin settings",
      "settings.plugins.configTitle": "{name} · settings",
      "settings.plugins.saveFailed": "Couldn't save: {msg}",
      "settings.plugins.range": "Between {min} and {max}.",
      "settings.plugins.min": "At least {min}.",
      "settings.plugins.max": "At most {max}.",
      "settings.plugins.actionModalTitle": "Plugin action",
      "settings.plugins.actionStarting": "Starting…",
      "settings.plugins.actionStartFailed": "Couldn't start the action: {msg}",
      "settings.plugins.actionExpired": "Your login expired (the service may have restarted). Reload the page and sign in again.",
      "settings.plugins.actionPollFailed": "Couldn't read the progress: {msg}",
      "settings.plugins.actionRetrying": "(Lost the connection to the service, retrying…)",

      // ---- 访问控制 ----
      "settings.access.title": "Access",
      "settings.access.desc": "Requests from this machine skip the login; anything else needs the password. The password lives in <config dir>/auth.json, not in config.yaml.",
      "access.current": "Current password",
      "access.new": "New password",
      "access.newDesc": "At least 8 characters. Save it empty to remove the password, which is allowed only while the service listens on this machine alone.",
      "access.confirm": "Confirm new password",
      "access.logout": "Sign out",
      "access.envManaged": "The password comes from the WEN_AUTH_PASSWORD environment variable and cannot be changed here.",
      "access.noPassword": "No password set. The service listens on this machine only — an external listen address is downgraded until a password exists. Set one and restart to serve other hosts.",
      "access.allLogin": "Password set. Every request, including from this machine, has to sign in.",
      "access.loopbackFree": "Password set. Requests from this machine skip the login; anything else has to sign in.",
      "access.localOnly": "Password set, but the service listens on this machine only, so nothing outside can reach it.",
      "access.stateFailed": "Couldn't read the access settings: {msg}",
      "access.mismatch": "The two new passwords don't match",
      "access.saveFailed": "Couldn't save",
      "access.saveFailedMsg": "Couldn't save: {msg}",

      // ---- 模型配置 ----
      "settings.models.title": "Models",
      "settings.models.desc": "Changes go to <config dir>/models.json and take effect from the next request on.",
      "models.addProvider": "＋ Add provider",
      "models.loadFailed": "Couldn't load the model settings: {msg}",
      "models.current": "In use: ",
      "models.currentNone": "none",
      "models.fromConfig": "from config file",
      "models.editProvider": "Edit provider",
      "models.deleteProvider": "Delete provider",
      "models.metaWithKey": "{url}  ·  API key {masked}",
      "models.metaNoKey": "{url}  ·  no API key",
      "models.empty": "No models yet",
      "models.addModel": "＋ Add model",
      "models.inUse": "Currently in use",
      "models.switchTo": "Click to switch to this model",
      "models.editModel": "Edit model",
      "models.deleteModel": "Delete model",
      "models.switchFailed": "Couldn't switch model: {msg}",
      "models.deleteProviderConfirm": "Delete the provider “{name}”?",
      "models.deleteModelConfirm": "Delete the model “{id}”?",
      "models.deleteFailed": "Couldn't delete: {msg}",

      "preset.modalTitle": "Choose a provider",
      "preset.hint": "Pick one and the address and common models are filled in for you — all you add is an API key.",
      "preset.custom": "Custom",
      "preset.customDesc": "Not listed above? Enter the address and settings yourself.",
      "preset.getKey": "Get an API key from {name}",
      "preset.noKeyNeeded": "Runs on this machine — no key needed",

      "provider.modalTitle": "Provider",
      "provider.edit": "Edit provider · {name}",
      "provider.add": "Add provider",
      "provider.name": "Name",
      "provider.nameDesc": "The name shown in the list. Has to be unique.",
      "provider.type": "API mode",
      "provider.typeDesc": "Anthropic mode uses the Messages API; OpenAI-compatible covers DeepSeek and similar services.",
      "provider.dialect": "Thinking parameter dialect",
      "provider.dialectDesc": "OpenAI-compatible services spell the thinking parameters differently: DeepSeek uses thinking+reasoning_effort, MiniMax adaptive+reasoning_split, Qwen enable_thinking. The wrong one is rejected by the API with a 400, or leaks the thinking into the reply.",
      "provider.baseUrl": "Base URL",
      "provider.baseUrlDesc": "The service address. Has to start with http:// or https://.",
      "provider.apiKey": "API key",
      "provider.apiKeyDesc": "Kept in models.json on this machine; never committed to the repository.",
      "provider.apiKeyPlaceholder": "Leave blank to keep the current key ({masked})",
      "provider.cache": "Prompt caching",
      "provider.cacheDesc": "Marks the unchanging part (tools, system prompt, existing history) as cacheable; on a hit that part bills at about a tenth of the price. A miss costs about a quarter extra to write, so if your messages are usually minutes apart — longer than the cache lives — turning this off is cheaper.",
      "provider.test": "Test connection",
      "provider.testNeedModel": "Add a model to this provider before testing",
      "provider.testing": "Testing…",
      "provider.testOk": "The connection works (model {model})",
      "provider.saveFailed": "Couldn't save: {msg}",

      "model.modalTitle": "Model",
      "model.edit": "Edit model · {id}",
      "model.add": "Add model · {provider}",
      "model.id": "Model ID",
      "model.idDesc": "The id sent to the API, for example deepseek-flash.",
      "model.name": "Display name",
      "model.nameDesc": "The name shown in the list. Leave it blank to show the model ID.",
      "model.context": "Context window",
      "model.contextDesc": "In tokens; drives trimming and the automatic compaction threshold. Leave blank to use the global value from config.yaml.",
      "model.maxTokens": "Max output tokens",
      "model.maxTokensDesc": "The output limit for one reply. Leave blank to use the global value.",
      "model.thinking": "Thinking",
      "model.thinkingDescAnthropic": "In Anthropic mode every level other than off maps to adaptive + effort.",
      "model.thinkingDesc": "off turns thinking off; the rest set how hard it thinks.",
      "model.temperature": "temperature",
      "model.temperatureDescAnthropic": "Ignored in Anthropic mode: current-generation Claude models take no sampling parameters.",
      "model.temperatureDesc": "0 to 2. Has no effect while thinking is on. Leave blank to use the global value.",
      "model.followGlobal": "Global default ({value})",
      "model.saveFailed": "Couldn't save: {msg}",
    },
  };

  // 只认这两种语言；其余一律英文。
  const LOCALES = { zh: "zh-CN", en: "en" };

  // detect 按 navigator.languages 的顺序找第一个认得的语言。用整个列表而不是
  // navigator.language：排在最前的可能是本项目没有的第三种语言，而用户在后面
  // 还列了中文。
  function detect() {
    const list = (navigator.languages && navigator.languages.length)
      ? navigator.languages
      : [navigator.language || ""];
    for (const raw of list) {
      const code = String(raw).toLowerCase();
      // zh-TW / zh-HK 也归中文：繁体读者读简体比读英文近
      if (code === "zh" || code.startsWith("zh-")) return "zh";
      if (code === "en" || code.startsWith("en-")) return "en";
    }
    return "en";
  }

  function readSetting() {
    let v = null;
    try {
      v = localStorage.getItem("wen-lang");
    } catch {
      // 隐私模式下取不到，按默认走
    }
    return v === "zh" || v === "en" ? v : "auto";
  }

  let setting = readSetting();
  let lang = setting === "auto" ? detect() : setting;

  function format(tpl, vars) {
    if (typeof tpl === "function") return tpl(vars || {});
    if (!vars) return tpl;
    return tpl.replace(/\{(\w+)\}/g, (m, k) =>
      Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m);
  }

  // t 取一条文案。缺键时回落中文并在控制台留一行——静默显示空白的话，漏译要等到
  // 有人截图才会发现。
  function t(key, vars) {
    const table = DICT[lang] || DICT.zh;
    let tpl = table[key];
    if (tpl === undefined) {
      tpl = DICT.zh[key];
      if (tpl === undefined) {
        console.warn("i18n: 未知文案键", key);
        return key;
      }
      console.warn("i18n: 缺少译文", lang, key);
    }
    return format(tpl, vars);
  }

  // 属性版的键：正文用 data-i18n，其余每个属性一个。textContent 会清空子节点，
  // 因此带图标的按钮把 data-i18n 挂在里面的 <span> 上，外层只挂属性键。
  const ATTR_KEYS = [
    ["data-i18n-title", "title"],
    ["data-i18n-placeholder", "placeholder"],
    ["data-i18n-aria-label", "aria-label"],
  ];
  const SELECTOR = "[data-i18n]," + ATTR_KEYS.map(([a]) => "[" + a + "]").join(",");

  function applyTo(el) {
    if (!el.getAttribute) return;
    const key = el.getAttribute("data-i18n");
    if (key) el.textContent = t(key);
    for (const [attr, target] of ATTR_KEYS) {
      const k = el.getAttribute(attr);
      if (k) el.setAttribute(target, t(k));
    }
  }

  function apply(root) {
    const node = root || document;
    if (node.nodeType === 1) applyTo(node);
    for (const el of node.querySelectorAll(SELECTOR)) applyTo(el);
  }

  function syncHtmlLang() {
    document.documentElement.lang = LOCALES[lang];
  }

  const listeners = [];

  const I18N = {
    lang,
    setting,
    locale: LOCALES[lang],
    t,
    apply,
    // set 切到某一档（auto / zh / en）并就地重刷界面，不需要刷新页面。
    set(next) {
      if (next !== "auto" && next !== "zh" && next !== "en") return;
      setting = next;
      try {
        localStorage.setItem("wen-lang", next);
      } catch {
        // 存不下也让本次切换生效，只是下次打开回到默认
      }
      lang = setting === "auto" ? detect() : setting;
      I18N.lang = lang;
      I18N.setting = setting;
      I18N.locale = LOCALES[lang];
      syncHtmlLang();
      apply(document);
      for (const fn of listeners) {
        try {
          fn();
        } catch (e) {
          console.error("i18n: 重绘回调出错", e);
        }
      }
    },
    onChange(fn) {
      listeners.push(fn);
    },
    // list 把若干名字连成一句里的并列成分：中文用顿号，英文用逗号。
    list(items) {
      return (items || []).join(lang === "zh" ? "、" : ", ");
    },
    // num 按当前语言的习惯给数字加千分位。
    num(n) {
      return Number(n).toLocaleString(LOCALES[lang]);
    },
  };

  window.I18N = I18N;
  window.t = t;

  syncHtmlLang();

  // 解析过程中就地填字：挂了键的元素在 HTML 里是空的，元素一进 DOM 就译好，因此
  // 永远不会先画出一屏旧语言。观察器在 DOMContentLoaded 上撤掉——之后消息区每秒
  // 都在增删节点，没必要一直听着。
  const observer = new MutationObserver((records) => {
    for (const rec of records) {
      for (const node of rec.addedNodes) {
        if (node.nodeType === 1) apply(node);
      }
    }
  });
  observer.observe(document.documentElement, { childList: true, subtree: true });
  document.addEventListener("DOMContentLoaded", () => {
    observer.disconnect();
    apply(document);
  });
})();
