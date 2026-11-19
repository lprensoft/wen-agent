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

## 三、Brave【待核对】

> 文档站 `api-dashboard.search.brave.com` 对普通抓取返回 403，本次只从它的渲染摘要里读到了端点、鉴权头与时间过滤的取值，**响应字段没有读到正文**，接之前必须复核。

- 端点：`GET https://api.search.brave.com/res/v1/web/search?q=…&freshness=pw`，请求头 `X-Subscription-Token: <key>`。
- `freshness`：`pd`（24 小时）/ `pw`（7 天）/ `pm`（31 天）/ `py`（一年），也支持自定义日期区间。
- 结果按公开资料在 `web.results[]`，字段 `title`、`url`、`description`（带 `<b>` 高亮）、`age`（人话，如 `2 days ago`）、`page_age`（ISO 时间）【待核对】。另有面向模型的 LLM Context 端点，本次不接。

## 四、接入的结论

- 三家塞进同一个 `Searcher` 接口，`hit` 只留标题、链接、摘要、发布日期四个字段。工具、缓存、限流与渲染都不必知道是哪一家。
- **`api_key` 由 Tavily 与 Brave 共用**，`base_url` 只给 SearXNG。三家不会同时用，多留两个输入框只会让设置页永远有一半是空的。
- **时间过滤按家映射**：Tavily 直接用；Brave 转 `pd` / `pw` / `pm` / `py`；SearXNG 没有 `week`，退一档成 `month`——宽一档是诚实的折衷，退成「不限时间」就是另一件事。
- **日期取 ISO 那一栏**。Brave 的 `age` 是「2 days ago」这种人话，渲染出来挂在标题后面没有意义，取 `page_age`；取不到就写「日期未明」。
- **缺凭据不回落**。选了 Brave 没填 key、选了 SearXNG 没填地址，都直接回一句指名道姓的话。静默回落到 Tavily 等于把人的查询发到一家他没选的服务上——那是隐私问题，不是体验问题。
- **SearXNG 的地址不套公网限制**。`fetch_url` 那套只连公网的检查是给模型给的地址用的；实例地址是人自己填的，跑在 127.0.0.1 或内网都正当。
