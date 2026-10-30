# AGENTS.md

本仓库的工程规范只维护一份：`CLAUDE.md`。开始任何改动前先读它，尤其是「Git 工作流约定」「常用命令」以及与改动相关的机制条目；本文件与 `CLAUDE.md` 冲突时，以 `CLAUDE.md` 为准。

## 最容易踩的坑

- **Windows 跑全量测试**：必须把临时构建放进项目目录，否则 `execcmd` 会被杀毒软件误杀成 `exit status 0xffffffff`。

  ```bash
  mkdir -p .gotmp && GOTMPDIR="$PWD/.gotmp" go test ./...
  ```

- **gofmt 检查**：工作区是 CRLF、库里是 LF；先去掉 `\r` 再比较，别把 `gofmt -l ./...` 的输出当信号。
- **分支与提交**：功能开发先建 `feature/<功能名>`；提交信息用中文，写清做了什么、为什么；推送前做敏感信息检查。
- **文案 / 配置 / 日志**：改了配置项、文案、提示词或日志，跑 `TestLocaleCoverage` 与 `TestPromptCoverage`。
- **CHANGELOG**：只记使用者能感知的变化；内部重构、测试与依赖调整不写。

规范细节不要复制到这里，去 `CLAUDE.md` 查原文。
