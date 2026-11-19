# 联网搜索的三家服务：Tavily / Brave / SearXNG

[← 返回文档索引](../README.md)　·　调研记录，只有中文

> **核对日期 2026-11-19。** 对象是 `web_search` 插件要接的三家搜索服务。记录的是「协议现在长什么样、接入要付什么代价」，不是路线图承诺；协议会变，据此动手前先核对原文。
>
> **证据等级：**
>
> - **【原文】** 直接读到官方文档正文。
> - **【待核对】** 文档站对普通抓取返回 403 或依赖前端渲染，本次没有读到正文，必须复核后才能当真。

## 一、Tavily【原文】

- 端点：`POST https://api.tavily.com/search`，`Content-Type: application/json`。
- 鉴权两条路：`Authorization: Bearer tvly-…`；或者不带 key，加 `X-Tavily-Access-Mode: keyless`。官方写明 keyless 与带 key 的**成功响应逐字段相同**，只是限流更紧——这条决定了插件「不填 key 也能用」。
- 请求参数：`query`、`max_results`（0–20，默认 10）、`search_depth`（`basic` / `advanced` / `fast` / `ultra-fast`，`advanced` 花两个 credit）、`time_range`（`day` / `week` / `month` / `year`）、`topic`（`general` / `news` / `finance`）、`include_answer`、`include_raw_content`、`include_published_date`（beta；`topic=news` 时自动开）。
- 响应：`results[]`，每条 `title`、`url`、`content`、`score`，开了 `include_published_date` 才有 `published_date`（形如 `Tue, 11 Mar 2025 17:00:00 GMT`，取不到时为 null）。

## 二、SearXNG【原文】

- 端点：`GET`（或 `POST`）`{instance}/search`。参数：`q`（必填）、`format`、`language`、`pageno`、`time_range`、`safesearch`。
- 要 JSON 得让实例在 `settings.yml` 的 `search.formats` 里开 `json`，否则**回 403**。官方明确说公共实例多数把这类格式关着——这是接 SearXNG 最常踩的一脚，403 因此不能笼统当成「密钥不对」。
- 只有 `format: json` 时响应才有 `results[]`：`url`、`title`、`content`、`publishedDate`（很常见为 null）、`engine`、`score`。
- `time_range` **只有 `day` / `month` / `year`，没有 `week`**。
- **上游故障也是 HTTP 200**：哪个引擎超时、被验证码拦、解析失败，都写在 `unresponsive_engines` 里（元素是 `[引擎, 译文]`），而 `results` 可能是空的。官方源码的 `get_json_response` 把它和 `results` 放在同一个字典里返回。当成「没搜到」会让模型照着编一句「网上没有」，调用方还会按成功记账——所以「结果为空且这个字段非空」必须报成故障。

## 三、Brave【原文（官方仓库源码）】

> 文档站 `api-dashboard.search.brave.com` 对普通抓取返回 403。字段名改从 Brave 自己的 MCP 服务器仓库核对：`brave/brave-search-mcp-server` 的 `src/tools/web/types.ts` 里有完整的 `WebSearchApiResponse` 类型（核对日期 2026-11-19）。端点、鉴权头与时间过滤的取值来自文档站的渲染摘要。

- 端点：`GET https://api.search.brave.com/res/v1/web/search?q=…&freshness=pw`，请求头 `X-Subscription-Token: <key>`。
- `freshness`：`pd`（24 小时）/ `pw`（7 天）/ `pm`（31 天）/ `py`（一年），也支持自定义日期区间。
- 响应顶层：`web?: Search`，其中 `Search { type, results: SearchResult[], family_friendly }`。`SearchResult` 继承 `Result`，字段是：
  - `title: string`、`url: string`、`description?: string`（带 `<b>` 高亮）
  - `page_age?: string`（官方注释：*A date representing the age of the web page*）
  - `page_fetched?: string`（抓取时刻，不是发布日，别拿来当日期）
  - `age?: string`（*A string representing the age*，形如 `2 days ago` / `August 7, 2022`）
  - 另有 `language`、`meta_url`、`profile`、`thumbnail`、`extra_snippets` 等
- **`page_age` 不带时区**：一个第三方集成的示例是 `"2022-08-07T03:07:47"`，不是 RFC 3339。按 `2006-01-02T15:04:05` 解析；少这一条布局不会报错，只会把原始时间串整段挂进标题行。
- 查询上限（同一个仓库的 `src/tools/web/params.ts`）：400 字符、50 个词。
- 另有面向模型的 LLM Context 端点，本次不接。

## 四、接入的结论

- 三家塞进同一个 `Searcher` 接口，`hit` 只留标题、链接、摘要、发布日期四个字段。工具、缓存、限流与渲染都不必知道是哪一家。
- **`api_key` 由 Tavily 与 Brave 共用**，`base_url` 只给 SearXNG。三家不会同时用，多留两个输入框只会让设置页永远有一半是空的。
- **时间过滤按家映射**：Tavily 直接用；Brave 转 `pd` / `pw` / `pm` / `py`；SearXNG 没有 `week`，退一档成 `month`——宽一档是诚实的折衷，退成「不限时间」就是另一件事。
- **日期取 ISO 那一栏**。Brave 的 `age` 是「2 days ago」这种人话，渲染出来挂在标题后面没有意义，取 `page_age`；取不到就写「日期未明」。
- **缺凭据不回落**。选了 Brave 没填 key、选了 SearXNG 没填地址，都直接回一句指名道姓的话。静默回落到 Tavily 等于把人的查询发到一家他没选的服务上——那是隐私问题，不是体验问题。
- **SearXNG 的地址不套公网限制**。`fetch_url` 那套只连公网的检查是给模型给的地址用的；实例地址是人自己填的，跑在 127.0.0.1 或内网都正当。
