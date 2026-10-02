# 部署与访问控制

[← 返回 README](../README.md)　·　中文　·　[English](en/deployment.md)

## 远程访问与访问控制

部署到远程服务器时，Web UI 的未授权访问不只是「聊天记录被看到」：`exec_command` 能执行命令、`read_file` 能读任意文件，而改掉某个提供商的 `base_url` 再触发一次请求就能把 api_key 送到别处。所以它默认只监听 `127.0.0.1`。

**认证按请求来源判定，不按监听地址判定**：

| 来源 | 行为 |
|---|---|
| 回环地址（`127.0.0.1` / `::1`） | 免认证。本机运行全程无感，不需要设任何口令 |
| 其它来源 | 需要访问口令，登录后由 Cookie 维持会话（7 天滑动过期，进程重启失效） |

口令存于 `<配置目录>/auth.json`（0600，只存 PBKDF2-HMAC-SHA256 派生值，不存明文），**不写进 config.yaml**——那个文件永不回写。

### 新装的机器怎么设第一个口令

配了对外监听却还没设口令时，服务**不会拒绝启动，而是降级为只监听 `127.0.0.1`** 并在日志里说明。这样永远不会出现「服务不可达且没法配置」的死局，同时一次都没有暴露过。补口令有三条路：

```bash
# 1）在服务器上直接配（推荐）
wen config server

# 2）SSH 隧道 + Web UI：隧道进来的请求源是回环，免认证，
#    打开界面后在设置页的「访问控制」里设置口令
ssh -L 8080:127.0.0.1:8080 <用户>@<服务器>

# 3）容器 / 自动化部署：环境变量优先于 auth.json，且界面上改不动
WEN_AUTH_PASSWORD=<口令> wen
```

设好口令后重启，配置里的 `host` 才会真正生效。

### 几条要注意的

- **套反向代理必须把 `server.trust_loopback` 关掉**。那种部署下所有请求都从回环地址进来，开着等于没有认证。程序刻意不读 `X-Forwarded-For`：请求头谁都能伪造，拿它当认证判据比不做认证还危险。
- **TLS 不由本程序提供**。明文 HTTP 上传口令等于没有认证，请让它跑在 SSH 隧道、WireGuard/Tailscale 组网或反向代理之后。
- **跨站写请求一律拒绝**（校验 `Origin` 与 `Host` 一致），本机部署同样受这层保护——否则任意网站都能让你的浏览器向 `127.0.0.1:8080` 发起写请求。
- 登录失败按来源 IP 限速（5 分钟内 5 次）。
- 改口令会让所有已登录会话失效，包括发起修改的那个。

### 升级提示（破坏性变更）

此前配了 `host: 0.0.0.0` 的实例，升级后若未设置口令会降级为只监听本机。这是有意的：那些实例此前处于完全开放状态。按上面任一条设置口令并重启即可恢复对外服务。

## 服务器部署（Linux）

两种方式：临时或简单场景用启停脚本，长期跑用 systemd（开机自启、崩溃自动拉起、journald 管日志）。两者的共同点：**用 `-c` 把配置目录钉死**。`-c` 指向的 `config.yaml` 不存在也没关系（全部取默认值），它所在的目录就是配置目录，模型配置、会话与插件状态都落在那里。不钉死的话，当前目录没有 `config.yaml` 时会落到运行用户的 `~/.wen/`，换个用户或换个目录启动，表现就是「像全新安装一样什么配置都没有」。

服务在后台跑时没有终端可以提问，所以**第一次启动之前**先在终端里用同一个 `-c` 跑一次首次运行向导，把模型配好（见[命令行](cli.md#wen-onboard首次运行向导)）。

### 启停脚本

放在 wen 二进制同目录，保存为 `wen.sh` 并 `chmod +x wen.sh`。第一次启动前先在这个目录里执行 `./wen onboard -c ./config.yaml` 配好模型：

```sh
#!/bin/sh
# wen 启停脚本：./wen.sh {start|stop|restart|status}

cd "$(dirname "$0")" || exit 1

PIDFILE=./wen.pid
LOGFILE=./wen.log

running() {
    [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

start() {
    if running; then
        echo "wen 已在运行（pid $(cat "$PIDFILE")）"
        exit 0
    fi
    nohup ./wen -c ./config.yaml >>"$LOGFILE" 2>&1 &
    echo $! >"$PIDFILE"
    sleep 1
    if running; then
        echo "wen 已启动（pid $(cat "$PIDFILE")），日志: $LOGFILE"
    else
        rm -f "$PIDFILE"
        echo "启动失败，最近日志："
        tail -n 20 "$LOGFILE"
        exit 1
    fi
}

stop() {
    if ! running; then
        echo "wen 没有在运行"
        rm -f "$PIDFILE"
        return 0
    fi
    pid=$(cat "$PIDFILE")
    kill "$pid"
    # 等最多 10 秒让它优雅收尾（停插件、断长连接）
    i=0
    while [ $i -lt 10 ] && kill -0 "$pid" 2>/dev/null; do
        sleep 1
        i=$((i + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
        echo "未在 10 秒内退出，强制结束"
        kill -9 "$pid"
    fi
    rm -f "$PIDFILE"
    echo "wen 已停止"
}

case "$1" in
    start)   start ;;
    stop)    stop ;;
    restart) stop; start ;;
    status)
        if running; then
            echo "wen 运行中（pid $(cat "$PIDFILE")）"
        else
            echo "wen 未运行"
        fi
        ;;
    *)
        echo "用法: $0 {start|stop|restart|status}"
        exit 1
        ;;
esac
```

日志会一直追加到 `wen.log`，久跑记得定期清理或配 logrotate。

### systemd

假设部署在 `/opt/wen`（按实际路径替换）。保存为 `/etc/systemd/system/wen.service`：

```ini
[Unit]
Description=Wen Agent
# 等网络真正可用再启动：QQ/微信长连接、模型接口都要出网
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=wen
Group=wen
WorkingDirectory=/opt/wen
ExecStart=/opt/wen/wen -c /opt/wen/config.yaml

# 崩溃自动拉起；正常 stop 不算失败
Restart=on-failure
RestartSec=5

# 优雅停止：默认发 SIGTERM，给 15 秒收尾（停插件、断长连接），超时才 SIGKILL
TimeoutStopSec=15

# 需要环境变量时在这里给：
# Environment=WEN_AUTH_PASSWORD=xxxx
# 或放进单独的文件（记得 chmod 600）：
# EnvironmentFile=/opt/wen/wen.env

# 可选加固。别开太狠：exec_command 插件本来就要执行命令、读写工作目录，
# 沙箱开满等于把功能关了。下面这组是不影响功能的底线：
NoNewPrivileges=true
ProtectSystem=full
ReadWritePaths=/opt/wen

[Install]
WantedBy=multi-user.target
```

自更新与这两种部署方式都相容：Linux 上的重启是原地换掉进程映像（PID 不变），启停脚本的 pid 文件仍然有效，systemd 也察觉不到一次退出，不会被 `Restart=` 抢先拉起。前提是那个二进制对运行它的用户可写——上面的 `chown -R wen:wen /opt/wen` 加 `ReadWritePaths=/opt/wen` 正好满足；二进制若放在 root 拥有的目录里，设置页上的更新会在第一步就停下并提示用原来的方式升级。

首次部署：

```sh
# 建专用用户（无登录 shell），并把目录交给它
sudo useradd --system --home /opt/wen --shell /usr/sbin/nologin wen
sudo chown -R wen:wen /opt/wen

# 以同一用户、同一个 -c 配好模型（交互式向导，只此一次）
sudo -u wen /opt/wen/wen onboard -c /opt/wen/config.yaml

sudo systemctl daemon-reload
sudo systemctl enable --now wen
```

日常操作：

```sh
sudo systemctl status wen          # 状态
sudo systemctl restart wen         # 重启
journalctl -u wen -f               # 跟日志
journalctl -u wen --since today    # 看今天的日志
```

两点注意：

- 用了 `User=wen` 之后，终端配置工具要以同一用户、同一个 `-c` 执行才能连上运行中的实例：`sudo -u wen /opt/wen/wen config -c /opt/wen/config.yaml`。
- `config.yaml` 若放在 `/opt/wen` 之外，把那个目录一并加进 `ReadWritePaths`。

