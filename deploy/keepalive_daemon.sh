#!/bin/sh
# hwcloud 容器内保活守护进程
#
# 为什么不用 cron：镜像里没装 cron，也不该为这一个任务装。
# 为什么不用 systemd：容器内 PID 1 是 entrypoint.sh，没有 systemd。
# 所以用一个最朴素的 while 循环 + sleep。
#
# 做的事：每 INTERVAL 秒对每个已登录账号跑一次 hwcloud keepalive。
# keepalive 内部逻辑：会话有效就直接返回（开销极小，
# 只是一次 HTTP 探测）；ECO_CN_ID 这类短效 cookie 过期时，
# 访问 grow 页让 SSO 用 hwid_cas_sid（有效期约 400 天）重新签发。

export HWCLOUD_HOME=/opt/hwcloud-demo
export PATH=/opt/hwcloud-demo/go/bin:$PATH
BIN=/opt/hwcloud-demo/go/bin/hwcloud
LOG=/opt/hwcloud-demo/logs/keepalive-daemon.log
INTERVAL=${KEEPALIVE_INTERVAL:-1200}   # 默认 20 分钟

mkdir -p "$(dirname "$LOG")"

log() { echo "[$(date "+%Y-%m-%d %H:%M:%S")] $*" >> "$LOG"; }

# 日志超过 500 行就截断，避免无限增长
trim_log() {
    if [ -f "$LOG" ] && [ "$(wc -l < "$LOG")" -gt 500 ]; then
        tail -n 300 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
    fi
}

log "保活守护启动，间隔 ${INTERVAL}s"

while true; do
    trim_log
    for d in /opt/hwcloud-demo/accounts/*/; do
        [ -d "$d/state" ] || continue
        id=$(basename "$d")
        # 只处理真的有会话文件的账号
        [ -f "$d/state/cookies.json" ] || continue

        out=$(HWCLOUD_ACCOUNT="$id" timeout 180 "$BIN" keepalive 2>&1)
        rc=$?
        # 压缩成一行，避免日志爆炸
        summary=$(echo "$out" | tr -d "\n" | sed "s/  */ /g" | cut -c1-200)
        if [ $rc -ne 0 ]; then
            log "[$id] 失败(rc=$rc): $summary"
        else
            case "$summary" in
                *"无需刷新"*) : ;;   # 会话本来就有效，不记日志免得刷屏
                *) log "[$id] $summary" ;;
            esac
        fi
    done
    sleep "$INTERVAL"
done
