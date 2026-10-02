# Deployment and access control

[← Back to README](../../README.en.md)　·　[中文](../deployment.md)　·　English

## Remote access and access control

On a remote server, unauthorized access to the web UI means considerably more than someone reading your chat logs: `exec_command` runs commands, `read_file` reads any file, and changing a provider's `base_url` and then triggering one request is enough to send your api_key somewhere else. So it listens on `127.0.0.1` only by default.

**Authentication is decided by where the request came from, not by what address is being listened on:**

| Origin | Behaviour |
|---|---|
| Loopback (`127.0.0.1` / `::1`) | No authentication. Running locally, you never notice it and never need to set a password |
| Anything else | An access password is required; a cookie keeps you signed in afterwards (7-day sliding expiry, invalidated by a restart) |

The password is stored in `<config dir>/auth.json` (mode 0600, holding only a PBKDF2-HMAC-SHA256 derivation, never the plaintext) and **not** in config.yaml — that file is never written back.

### Setting the first password on a fresh machine

If you have configured a non-local listen address but not yet set a password, the service **does not refuse to start; it falls back to listening on `127.0.0.1` only** and says so in the log. That way you can never end up with a service that is unreachable and impossible to configure, and it has not been exposed for a single moment. There are three ways to add the password:

```bash
# 1) Set it on the server itself (recommended)
wen config server

# 2) SSH tunnel plus the web UI: a request coming through the tunnel
#    originates from loopback and needs no authentication, so open the UI
#    and set the password under "Access control" on the settings page
ssh -L 8080:127.0.0.1:8080 <user>@<server>

# 3) Containers and automated deployment: the environment variable wins over
#    auth.json, and the UI cannot change it
WEN_AUTH_PASSWORD=<password> wen
```

Restart after setting the password, and the `host` in your config finally takes effect.

### Things to watch out for

- **Behind a reverse proxy you must turn `server.trust_loopback` off.** In that kind of deployment every request arrives from loopback, so leaving it on is the same as having no authentication at all. The program deliberately does not read `X-Forwarded-For`: anyone can forge a header, and trusting one as proof of identity is worse than not authenticating.
- **TLS is not provided by this program.** Sending a password over plain HTTP is no authentication at all, so run it behind an SSH tunnel, a WireGuard/Tailscale network, or a reverse proxy.
- **Cross-site write requests are always refused** (`Origin` is checked against `Host`), and a local deployment gets that protection too — otherwise any website could make your browser issue write requests to `127.0.0.1:8080`.
- Failed sign-ins are rate-limited by source IP (5 within 5 minutes).
- Changing the password invalidates every signed-in session, including the one that made the change.

### Upgrade note (breaking change)

An instance that used to be configured with `host: 0.0.0.0` will, after upgrading, fall back to listening on localhost only if no password is set. This is deliberate: those instances were previously wide open. Set a password by any of the routes above and restart to serve remotely again.

## Server deployment (Linux)

Two ways: a start/stop script for temporary or simple cases, systemd for anything long-running (start on boot, restart on crash, logs in journald). What they have in common: **pin the config directory with `-c`**. The `config.yaml` that `-c` points at does not have to exist (everything then takes its default). Its directory is the config directory, where the model configuration, sessions and plugin state are stored. Without `-c`, a missing `config.yaml` in the current directory sends everything to the running user's `~/.wen/`. Start it as another user or from another directory and it looks exactly like a fresh install with no configuration at all.

A service running in the background has no terminal to ask questions in. So **before the first start**, run the setup wizard once in a terminal with the same `-c` to configure the model (see [CLI](cli.md#wen-onboard-the-setup-wizard)).

### A start/stop script

Put it beside the wen binary, save it as `wen.sh` and `chmod +x wen.sh`. Before the first start, run `./wen onboard -c ./config.yaml` in that directory to configure the model:

```sh
#!/bin/sh
# wen start/stop script: ./wen.sh {start|stop|restart|status}

cd "$(dirname "$0")" || exit 1

PIDFILE=./wen.pid
LOGFILE=./wen.log

running() {
    [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

start() {
    if running; then
        echo "wen is already running (pid $(cat "$PIDFILE"))"
        exit 0
    fi
    nohup ./wen -c ./config.yaml >>"$LOGFILE" 2>&1 &
    echo $! >"$PIDFILE"
    sleep 1
    if running; then
        echo "wen started (pid $(cat "$PIDFILE")), log: $LOGFILE"
    else
        rm -f "$PIDFILE"
        echo "failed to start, recent log:"
        tail -n 20 "$LOGFILE"
        exit 1
    fi
}

stop() {
    if ! running; then
        echo "wen is not running"
        rm -f "$PIDFILE"
        return 0
    fi
    pid=$(cat "$PIDFILE")
    kill "$pid"
    # give it up to 10 seconds to shut down gracefully (stop plugins, close connections)
    i=0
    while [ $i -lt 10 ] && kill -0 "$pid" 2>/dev/null; do
        sleep 1
        i=$((i + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
        echo "did not exit within 10 seconds, killing"
        kill -9 "$pid"
    fi
    rm -f "$PIDFILE"
    echo "wen stopped"
}

case "$1" in
    start)   start ;;
    stop)    stop ;;
    restart) stop; start ;;
    status)
        if running; then
            echo "wen is running (pid $(cat "$PIDFILE"))"
        else
            echo "wen is not running"
        fi
        ;;
    *)
        echo "usage: $0 {start|stop|restart|status}"
        exit 1
        ;;
esac
```

The log is appended to `wen.log` indefinitely, so remember to rotate or clear it if you leave it running for a long time.

### systemd

Assuming a deployment in `/opt/wen` (substitute your own path). Save as `/etc/systemd/system/wen.service`:

```ini
[Unit]
Description=Wen Agent
# Wait until the network is really up: the QQ/WeChat connections and the
# model endpoints all need to reach the internet
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=wen
Group=wen
WorkingDirectory=/opt/wen
ExecStart=/opt/wen/wen -c /opt/wen/config.yaml

# Restart on crash; a normal stop is not a failure
Restart=on-failure
RestartSec=5

# Graceful stop: SIGTERM by default, 15 seconds to wind down (stop plugins,
# close connections), SIGKILL only after that
TimeoutStopSec=15

# Environment variables go here when you need them:
# Environment=WEN_AUTH_PASSWORD=xxxx
# or in a separate file (remember chmod 600):
# EnvironmentFile=/opt/wen/wen.env

# Optional hardening. Do not overdo it: the exec_command plugin is meant to run
# commands and read and write the working directory, so a maximal sandbox just
# turns the feature off. The set below is the floor that costs no functionality:
NoNewPrivileges=true
ProtectSystem=full
ReadWritePaths=/opt/wen

[Install]
WantedBy=multi-user.target
```

Self-update works with either deployment. On Linux the restart replaces the process image in place (the PID does not change), so the pid file from the script stays valid and systemd does not observe an exit and jump in with `Restart=`. The one prerequisite is that the binary be writable by the user running it — the `chown -R wen:wen /opt/wen` plus `ReadWritePaths=/opt/wen` above satisfies exactly that. If the binary sits in a root-owned directory, the update on the settings page stops at the first step and tells you to upgrade the way you installed it.

First deployment:

```sh
# Create a dedicated user (no login shell) and hand it the directory
sudo useradd --system --home /opt/wen --shell /usr/sbin/nologin wen
sudo chown -R wen:wen /opt/wen

# Configure the model as the same user with the same -c (an interactive wizard, once)
sudo -u wen /opt/wen/wen onboard -c /opt/wen/config.yaml

sudo systemctl daemon-reload
sudo systemctl enable --now wen
```

Day-to-day:

```sh
sudo systemctl status wen          # state
sudo systemctl restart wen         # restart
journalctl -u wen -f               # follow the log
journalctl -u wen --since today    # today's log
```

Two notes:

- With `User=wen`, the terminal configuration tool has to run as the same user with the same `-c` to reach the running instance: `sudo -u wen /opt/wen/wen config -c /opt/wen/config.yaml`.
- If `config.yaml` lives outside `/opt/wen`, add that directory to `ReadWritePaths` as well.
