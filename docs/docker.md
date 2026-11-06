# 容器部署（Docker）

[← 文档首页](README.md)　·　[← 返回项目 README](../README.md)　·　中文　·　[English](en/docker.md)

镜像把程序、Web UI 与运行时依赖打在一起，适合 NAS、VPS 或已经用 Docker 管理服务的机器。只支持 Linux 容器。

## 快速开始

```bash
mkdir -p wen-data
sudo chown 10001:10001 wen-data
export WEN_AUTH_PASSWORD='换成一个足够长的口令'
docker compose up -d --build
```

打开 `http://127.0.0.1:8080`。第一次运行还没有可用的模型，可以运行一次向导：

```bash
docker compose run --rm -it wen onboard -c /data/config.yaml
```

`WEN_AUTH_PASSWORD` 是必填项。容器监听 `0.0.0.0`，而程序的安全规则是「对外监听但没有口令时降级回 127.0.0.1」——没有这个变量，容器起来了，容器外也访问不到。

数据目录的属主必须是 uid 10001（镜像以非 root 用户运行）。Docker 自动创建 `wen-data` 时属主是 root，程序会写不进去；Linux 上按上面的命令先建好并改属主，Docker Desktop 通常会自动处理。

## 预构建镜像与多架构

正式版本会同时发布 amd64 与 arm64 镜像，同一个 tag 按机器架构自动选择：

```bash
docker pull ghcr.io/lprensoft/wen-agent:latest
```

包默认是私有的；要在别的机器上匿名拉取，先在 GitHub 的包设置里改成 public，或者 `docker login ghcr.io` 后再拉。手动触发 “Docker image” workflow 会发布 `dev` 标签，用于发版前验证整条流水线。

compose 默认在本地构建。想用预构建镜像：

```bash
export WEN_IMAGE=ghcr.io/lprensoft/wen-agent:latest
docker compose pull
docker compose up -d
```

## 配置

| 变量 | 默认 | 说明 |
|---|---|---|
| `WEN_AUTH_PASSWORD` | 无（必填） | Web UI 访问口令 |
| `WEN_PORT` | `8080` | 映射到宿主机的端口；容器内始终监听 8080 |
| `TZ` | `Asia/Shanghai` | 时区；日程、日界线与天气都依赖本地时间 |
| `WEN_VERSION` | 空 | 构建时写入版本号，不填则使用源码里的版本 |
| `WEN_IMAGE` | `wen-agent:local` | 使用的镜像；用预构建镜像时设为 `ghcr.io/lprensoft/wen-agent:latest` |

配置文件、会话与插件数据都落在 `/data`，compose 把它映射到 `./wen-data`。备份就是备份这个目录。

## 更新

```bash
git pull
docker compose up -d --build
```

不要在容器里使用 `self_update`：它替换的是镜像内的二进制，容器重建后就会消失，而镜像里的 `/usr/local/bin` 对运行用户不可写。请在设置页把「程序维护 · 程序更新」关掉，升级统一走镜像。

## 安全

- 镜像监听 `0.0.0.0` 且要求口令，但仍然不要把端口直接暴露到公网。
- TLS 不由本程序提供。放到反向代理之后时，记得关掉 `server.trust_loopback`，见[部署与访问控制](deployment.md)。
- 容器内只有 Alpine 的基础环境。外源插件里的 MCP server 如果需要 node、python 等运行时，要基于本镜像自行安装，或改用 `streamable-http` 连到容器外的 server。
- `exec_command` 与 `read_file` 默认关闭。打开后容器内的文件与命令都对模型可见；以非 root 运行只能降低影响，不构成沙箱。

## 自己构建

```bash
docker build -t wen-agent:local .
docker build --build-arg VERSION=v1.0.0 -t wen-agent:v1.0.0 .
```

compose 默认使用本地构建的 `wen-agent:local`。要用预构建镜像，通过 `WEN_IMAGE` 覆盖，见上面「预构建镜像与多架构」。
