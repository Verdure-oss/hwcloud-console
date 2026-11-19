#!/bin/sh
# hwcloud 容器内保活守护进程
#
# 为什么不用 cron：镜像里没装 cron，也不该为这一个任务装。
# 为什么不用 systemd：容器内 PID 1 是 entrypoint.sh，没有 systemd。
# 所以用一个最朴素的 while 循环 + sleep。
#
# 做的事：每 INTERVAL 秒对每个已登录账号跑一次 hwcloud keepalive。
# keepalive 内部逻辑：会话有效就直接返回（开销极小，只是一次 HTTP 探测）；
# ECO_CN_ID 这类短效 cookie 过期时，访问 grow 页让 SSO 重新签发。
#
# ⚠ 关于「能撑多久」（2026-11-19 实测修正）：
#   hwid_cas_sid 的 expires 写着约 400 天，但那是**客户端记录**，
#   服务端有独立的会话失效策略。实测登录后仅 1 小时就被判定未登录
#   （grow 页 200 但 devdata 接口 403 not login）。
#   所以不要以为「cookie 没过期就还能用」，必须靠实际探测判断。

export HWCLOUD_HOME=/opt/hwcloud-demo
export PATH=/opt/hwcloud-demo/go/bin:$PATH
BIN=/opt/hwcloud-demo/go/bin/hwcloud
LOG=/opt/hwcloud-demo/logs/keepalive-daemon.log
MARK=/opt/hwcloud-demo/state/NEED_MANUAL_LOGIN
INTERVAL=${KEEPALIVE_INTERVAL:-1200}   # 默认 20 分钟

mkdir -p "$(dirname "$LOG")" "$(dirname "$MARK")"

log() { echo "[$(date "+%Y-%m-%d %H:%M:%S")] $*" >> "$LOG"; }

trim_log() {
    if [ -f "$LOG" ] && [ "$(wc -l < "$LOG")" -gt 500 ]; then
        tail -n 300 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
    fi
}

# 每个账号「上次报的错」记在临时文件里，内容一样就不再重复写日志。
# 为什么：失效后每 20 分钟刷一条同样的失败，4 天能刷出 200+ 条，
# 想看「哪天开始断的」反而找不着。
STATE_DIR=/tmp/.keepalive-state
mkdir -p "$STATE_DIR"

log "保活守护启动，间隔 ${INTERVAL}s"

while true; do
    trim_log
    any_dead=0

    for d in /opt/hwcloud-demo/accounts/*/; do
        [ -d "$d/state" ] || continue
        id=$(basename "$d")
        [ -f "$d/state/cookies.json" ] || continue

        out=$(HWCLOUD_ACCOUNT="$id" timeout 180 "$BIN" keepalive 2>&1)
        rc=$?
        summary=$(echo "$out" | tr -d "\n" | sed "s/  */ /g" | cut -c1-200)

        if [ $rc -ne 0 ]; then
            any_dead=1
            prev="$STATE_DIR/$id.err"
            # 同样的错误只记第一次
            if [ -f "$prev" ] && [ "$(cat "$prev")" = "$summary" ]; then
                :
            else
                echo "$summary" > "$prev"
                log "[$id] 失败(rc=$rc): $summary"
            fi
            # 首次失效写标记文件，方便随时查状态
            if [ ! -f "$MARK" ]; then
                {
                    echo "时间: $(date '+%Y-%m-%d %H:%M:%S')"
                    echo "账号: $id"
                    echo "原因: 会话已失效，自动续期失败"
                    echo ""
                    echo "处理方式（任选其一）："
                    echo "  1. 面板点账号卡片的「重新登录」（需已保存密码）"
                    echo "  2. 打开 https://hwcloud.vernon.de5.net/vnc/ 手动登录"
                    echo "  3. 让 AI 助手执行：curl -X POST http://127.0.0.1:8901/manual -d '{\"phone\":\"$id\"}'"
                    echo ""
                    echo "登录成功后此文件会自动删除。"
                } > "$MARK"
                log "[$id] 已写入标记 $MARK（需要人工登录）"
            fi
        else
            # 成功就清掉上次的错和标记
            rm -f "$STATE_DIR/$id.err"
            if [ -f "$MARK" ]; then
                rm -f "$MARK"
                log "[$id] 会话已恢复，标记已清除"
            fi
            case "$summary" in
                *"无需刷新"*) : ;;   # 会话本来就有效，不记日志免得刷屏
                *) log "[$id] $summary" ;;
            esac
        fi
    done

    # 所有账号都正常时清理可能残留的标记
    if [ $any_dead -eq 0 ] && [ -f "$MARK" ]; then
        rm -f "$MARK"
    fi

    sleep "$INTERVAL"
done
