# 回放评测（wen eval）

[← 返回 README](../README.md)　·　中文　·　[English](en/evaluation.md)


调一次提示词之后，角色是变好了还是变坏了？`wen eval` 把这件事从「再聊几句感觉一下」变成可以重复跑、能判定的脚本：

```bash
wen eval docs/eval/example.json                    # 报告打到标准输出
wen eval docs/eval/example.json -o 报告.md          # 写入文件
wen eval docs/eval/night.json --json run.json      # 结构化结果，供下一次对比
wen eval docs/eval/night.json --baseline run.json  # 与上次对比，确定性指标退化时退出码为 1
```

脚本是一个 JSON：`name` 给报告起名，`start` 给虚拟时钟一个起点（RFC3339，不写就是当下），`seed` 预置状态，`turns` 按顺序列出步骤——`{"say": "你说的话"}` 是一轮对话，`{"compact": true}` 在此处强制压缩一次（看压缩前后角色有没有变），`{"gap_hours": 8}` 把虚拟时钟向前拨八小时，下一轮就真的隔了八小时（历史里的间隔、心情的回落、日记的日界都按它算，系统时间不动）；可选的 `judge_points` 给评判加几条额外关注点。示例见 [`docs/eval/example.json`](eval/example.json)。

`seed` 在开跑前用插件自己的工具把状态写进去，写法和模型调用工具一样：

```json
"seed": [
  {"tool": "save_memory", "args": {"name": "怕冷", "description": "空调 26 度以下会冷", "type": "偏好", "content": "……"}},
  {"tool": "update_relationship", "args": {"stage": "熟悉"}, "scope": "inner", "ago_hours": 72}
]
```

`scope` 把状态写进某个人格，不写就是共享域；取值与插件名同规（小写字母开头的 `[a-z0-9_]`），因为它会被拼进数据目录，写错会在解析脚本时就报错。`ago_hours` 让这条状态写在现在之前多少小时——72 就是「三天前记住的」，淡忘、索引排序、快照里的「N 天前」都按这个时刻算。走真实工具意味着字段与插件文档一致，也不必担心落盘格式变化。

`expect` 挂在 `say` 上，是不依赖模型评分的确定性断言：

```json
{"say": "今天空调开好低。", "expect": {
  "must_include": ["空调"], "must_not_include": ["作为 AI"],
  "max_hits": {"closing_cliche": 0}, "max_sentences": 3, "must_call": ["recall_memory"]
}}
```

任何一条不满足就算「门禁未通过」，退出码为 1。`max_hits` 用 `style_watch` 的规则 id，拼错会在解析脚本时就报出来。`max_sentences` 不写或写 0 表示不限；`max_hits` 里的 0 则表示一次都不许命中。

它用与 `wen serve` **相同的配置与插件**（读同一份 config.yaml、models.json 与 plugins.state.json，角色设定、样例、自然表达规则全部照常生效），但：会话与插件数据全部落在一个临时目录、跑完即删，不碰真实数据；消息通道与后台任务类插件（心跳、定时、QQ / 微信 / 飞书 / Telegram）不启动。服务正在运行时也可以跑，两者互不影响。

报告分两半：上半是门禁与逐轮确定性指标（字数、句数、演绎占比、助手腔命中、断言结果），下半是模型评判（「像不像同一个人 / 语气是否一致 / 关系与称呼是否连续」，各 1-5 分）。给了 `--baseline` 时多一节对比：命中变多、句数明显变多、新增断言失败判为退化；**评分只列变化，不判失败**。对比前会先核对基线是不是同一串问题——轮数与每句 `say` 都对不上时直接报错，免得拿两道不同的题的数字硬比；改了脚本就用 `--json` 重新生成基线。

几条局限：评分是模型打的，同一脚本跑两次会抖，用来看趋势、不当绝对值；助手腔检测是正则启发式，命中不等于出戏；后台定时（心跳、日程到点、健康提醒）仍按系统时间跑，虚拟时钟管不到它们——它们驱动的是真实定时器；`seed` 只能通过 `ago_hours` 往过去放，表达不了「两天后生效」这类未来时刻。
