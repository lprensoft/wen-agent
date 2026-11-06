# 备份与恢复

[← 文档首页](README.md)　·　[← 返回项目 README](../README.md)　·　中文　·　[English](en/backup.md)

`wen export` 把配置目录里程序维护的数据打成归档，`wen import` 把它恢复到一个空目录。

## 备份

```bash
wen export
wen export -o ~/backups/wen-20261106.tar.gz
wen export -c /srv/wen/config.yaml -o /backup/wen.tar.gz
```

默认写到当前目录的 `wen-backup-<日期>-<时间>.tar.gz`，文件权限 0600。

**服务必须先停下。** 会话文件还在追加，在线导出的快照可能停在半行。服务运行时命令会拒绝；确实需要在线导出时加 `--force`，并自己承担不一致的风险。`--force` 同时允许覆盖已存在的输出文件。

包含的内容：

| 路径 | 内容 |
|---|---|
| `config.yaml` | 启动期配置（按 `-c` 指向的实际文件名） |
| `models.json` | 提供商、密钥与当前模型 |
| `plugins.state.json` | 插件开关与界面上保存的配置 |
| `auth.json` | 访问口令的 PBKDF2 派生值 |
| `sessions/` | 全部会话正文 |
| `plugins/` | 记忆、归档、心跳状态、消息通道 token 等 |
| `bundles/` | 外源插件包 |

不包含：

- `wen.lock`（运行实例登记）；
- 符号链接与设备文件等特殊文件；
- 配置目录之外的会话目录（`session.dir` 指向别处时会打印提示）；
- 配置目录之外的项目记忆：`memory` 插件的 `scope=project` 且工作目录在别处时打印提示并跳过；工作目录在配置目录内时，按可见域分出的 `memories-<标签>` 库会一并备份。
- 日志、构建产物与空目录。

**备份里有敏感信息。** `models.json` 含明文 API Key，插件状态里有消息通道凭据，`auth.json` 里有口令派生值，会话里有全部对话。文件权限是 0600，但仍请加密或放到安全的位置；不要提交到代码仓库。

## 恢复

```bash
wen import ~/backups/wen.tar.gz
wen import ~/backups/wen.tar.gz -c /srv/wen/config.yaml
```

目标目录是 `-c` 指向的配置文件所在目录。它**必须是空目录或还不存在**：命令不合并、不覆盖现有数据。要恢复到已有目录，先把原目录改名或移到别处。

服务运行时同样拒绝恢复。恢复完成后：

```bash
wen status -c /srv/wen/config.yaml
wen serve -c /srv/wen/config.yaml
```

备份来自不同程序版本时只打印提示，不阻止恢复；归档格式版本高于本程序支持的范围时拒绝。恢复时会校验 `models.json` 与 `plugins.state.json` 是合法 JSON，并核对 manifest 里的文件数与总字节数；损坏或不完整的归档不会落进目标目录。

## 归档结构

```text
manifest.json
data/config.yaml
data/models.json
data/plugins.state.json
data/auth.json
data/sessions/...
data/plugins/...
data/bundles/...
```

`manifest.json` 记录格式版本、程序版本、创建时间、源平台、配置文件名、文件数与总字节数。导入时只接受相对路径，拒绝 `..`、绝对路径、反斜杠、符号链接与硬链接；单个归档最多 20 万个条目、4 GiB。以上限与校验都在临时目录里完成，失败不会污染目标目录。
