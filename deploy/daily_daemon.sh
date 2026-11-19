#!/bin/sh
# hwcloud 每日任务自动调度守护
#
# 为什么不用 cron / systemd：
#   镜像是 debian-slim，没装 cron；容器内 PID 1 是 entrypoint.sh，没有 systemd。
#   所以用一个朴素的长驻循环 + sleep，靠比对「今天跑过没有」决定是否触发。
#
# 关键设计（照抄项目 systemd 脚本里踩过的坑）：
#   1. 随机延时：固定时刻、固定顺序都是明显的机器特征。
#      整轮启动抖 0-300s，账号之间抖动由 hwcloud all 内部串行保证。
#   2. 幂等：同一天只跑一次。用 stamp 文件记录，容器重启也不会重跑。
#   3. 失败不重试当天：避免反复触发风控。第二天自然重来。
#   4. 跳过已完成步骤由 hwcloud 自己判断（runDaily 里有 skipIfDone）。

export HWCLOUD_HOME=/opt/hwcloud-demo
export PATH=/opt/hwcloud-demo/go/bin:$PATH
export DISPLAY=:99

BIN=/opt/hwcloud-demo/go/bin/hwcloud
LOG=/opt/hwcloud-demo/logs/daily-daemon.log
STAMP_DIR=/opt/hwcloud-demo/logs/.daily-stamp
RUN_AT_HOUR=${DAILY_HOUR:-9}
RUN_AT_MIN=${DAILY_MINUTE:-30}
JITTER=${DAILY_JITTER:-300}      # 0-300 秒随机抖动

mkdir -p "$(dirname "$LOG")" "$STAMP_DIR"

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG"; }

trim_log() {
    if [ -f "$LOG" ] && [ "$(wc -l < "$LOG")" -gt 800 ]; then
        tail -n 400 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
    fi
}

# 启动时就已过点的情况：不要立刻补跑。
# 为什么：容器/守护进程可能在下午重启，此时补跑「今天的日常」没有意义
# （用户可能已经手点过了，而且补跑也拿不到当日名额）。直接标记今天已过，
# 从明天开始正常调度。想立即跑就手动执行 hwcloud all daily。
today0=$(date '+%Y-%m-%d')
now0=$(date '+%H%M')
target0=$(printf '%02d%02d' "$RUN_AT_HOUR" "$RUN_AT_MIN")
if [ "$now0" -ge "$target0" ] && [ ! -f "$STAMP_DIR/$today0" ]; then
    : > "$STAMP_DIR/$today0"
    log "启动时已过当日触发点（${RUN_AT_HOUR}:$(printf '%02d' "$RUN_AT_MIN")），今天不再自动跑，从明天开始"
fi

log "日常调度守护启动（每天 ${RUN_AT_HOUR}:$(printf '%02d' "$RUN_AT_MIN") + 0-${JITTER}s 抖动）"

while true; do
    trim_log

    today=$(date '+%Y-%m-%d')
    now_hm=$(date '+%H%M')
    target=$(printf '%02d%02d' "$RUN_AT_HOUR" "$RUN_AT_MIN")
    stamp="$STAMP_DIR/$today"

    # 到点且今天还没跑 → 跑
    if [ "$now_hm" -ge "$target" ] && [ ! -f "$stamp" ]; then
        # 先占位，避免这一轮 sleep 期间重复触发
        : > "$stamp"

        # 随机抖动，打散启动时刻
        jitter=$(( $(od -An -N2 -tu2 /dev/urandom) % JITTER ))
        log "到达触发时间，抖动 ${jitter}s 后开始日常"
        sleep "$jitter"

        # 先探一次会话。离线就整轮跳过 —— 否则 all daily 会对每个账号
        # 尝试一次自动登录，必然因「未保存密码 / 需验证码」失败，
        # 日志被几十条同样的报错刷满，看不出真正的问题。
        #
        # 判定方式用 `session` 而不是 `keepalive`：前者只做一次 HTTP 探测，
        # 不触发浏览器，开销小；后者在失效时会尝试拉起 profile 续期。
        alive=""
        for dd in /opt/hwcloud-demo/accounts/*/; do
            [ -f "$dd/state/cookies.json" ] || continue
            aid=$(basename "$dd")
            astate=$(HWCLOUD_ACCOUNT="$aid" timeout 60 "$BIN" session 2>/dev/null \
                     | tr -d '\n' | sed 's/  */ /g')
            case "$astate" in
                *'"alive":true'*|*'"alive": true'*) alive="yes" ;;
                *) alive="no" ;;
            esac
            break
        done

        if [ "$alive" = "no" ]; then
            if [ -f /opt/hwcloud-demo/state/NEED_MANUAL_LOGIN ]; then
                log "会话离线（已有 NEED_MANUAL_LOGIN 标记），跳过今日任务。请手动登录后重试"
            else
                log "会话离线，跳过今日任务。请手动登录（详见 /opt/hwcloud-demo/state/NEED_MANUAL_LOGIN）"
            fi
            sleep 300
            continue
        fi

        log "开始执行 hwcloud all daily"
        start=$(date +%s)
        out=$("$BIN" all daily 2>&1)
        rc=$?
        elapsed=$(( $(date +%s) - start ))

        # 结果压成一行便于查看
        summary=$(echo "$out" | tr -d '\n' | sed 's/  */ /g' | cut -c1-600)
        if [ $rc -eq 0 ]; then
            log "日常完成（${elapsed}s）: $summary"
        else
            log "日常失败（rc=$rc, ${elapsed}s）: $summary"
        fi
    fi

    # 每 5 分钟检查一次是否到点，足够准且开销极小
    sleep 300
done
