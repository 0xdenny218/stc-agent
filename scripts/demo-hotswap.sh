#!/usr/bin/env bash
# 对话进行中热替换 demo（spec M3 验收场景 E2E/HotSwapKeepsSession 的真人版）：
#   1. 构建 dice v1 → 启动 agent（真实模型，同一进程跑全程）；
#   2. 请模型掷骰子 → v1 结果；
#   3. 原地重建 v2（不重启、不丢会话）；
#   4. 再掷一次 → v2 结果。transcript 逐字为证。
set -euo pipefail
cd "$(dirname "$0")/.."

# --- 前置检查 -------------------------------------------------------
tinygo="${TINYGO:-}"
if [[ -z "$tinygo" ]]; then
	if command -v tinygo >/dev/null 2>&1; then
		tinygo=tinygo
	elif [[ -x "$HOME/.local/opt/tinygo/bin/tinygo" ]]; then
		tinygo="$HOME/.local/opt/tinygo/bin/tinygo"
	else
		echo "error: tinygo not found (set TINYGO or install to ~/.local/opt/tinygo)" >&2
		exit 1
	fi
fi
if [[ -z "${STC_AGENT_API_KEY:-}${DEEPSEEK_API_KEY:-}${OPENAI_API_KEY:-}" && ! -f "$HOME/.config/stc-agent/config.json" ]]; then
	echo "error: no API key (set STC_AGENT_API_KEY / DEEPSEEK_API_KEY, or write ~/.config/stc-agent/config.json)" >&2
	exit 1
fi

work="$(mktemp -d)"
agent_pid=""
cleanup() {
	[[ -n "$agent_pid" ]] && kill "$agent_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/tools.d"

echo "==> building dice v1"
"$tinygo" build -target wasip1 -buildmode=c-shared -o "$work/tools.d/dice.wasm" ./examples/guests/dice
echo "==> building stc-agent"
go build -o "$work/stc-agent" ./cmd/stc-agent

# --- 启动 agent -----------------------------------------------------
mkfifo "$work/stdin"
"$work/stc-agent" --tools-dir "$work/tools.d" --transcript "$work/chat.jsonl" \
	<"$work/stdin" >"$work/agent.log" 2>&1 &
agent_pid=$!
exec 3>"$work/stdin" # 持有写端：agent 不会因 stdin EOF 退出
echo "==> agent started (pid $agent_pid)"

say() { printf '%s\n' "$1" >&3; }

# wait_turn <消息数>：transcript 是事件日志（M5 起），按 message 事件
# 计数——攒够即本轮结束（usage 事件穿插其间，不计入）。
wait_turn() {
	local want="$1" deadline=$((SECONDS + 90)) n
	until [[ -f "$work/chat.jsonl" ]] && n=$(grep -c '"type":"message"' "$work/chat.jsonl" || true) && ((n >= want)); do
		if ((SECONDS > deadline)); then
			echo "error: timed out waiting for turn to finish; agent log:" >&2
			cat "$work/agent.log" >&2
			exit 1
		fi
		sleep 0.2
	done
}

# --- 第一轮：v1 -----------------------------------------------------
echo "==> turn 1: asking the model to roll (dice v1 on disk)"
say "Roll a die using the dice tool (call it with {}), then report the JSON result."
wait_turn 4
roll1=$(grep '"role":"tool"' "$work/chat.jsonl" | head -1)
echo "    tool result: $roll1"
echo "$roll1" | grep -qF '\"version\":\"v1\"' || {
	echo "error: turn 1 result is not v1" >&2
	exit 1
}

# --- 对话进行中热替换 ------------------------------------------------
echo "==> rebuilding dice.wasm in place as v2 (agent pid $agent_pid keeps running)"
"$tinygo" build -target wasip1 -buildmode=c-shared -tags v2 -o "$work/tools.d/dice.wasm" ./examples/guests/dice
deadline=$((SECONDS + 30))
until grep -qF '[guest] dice reloaded' "$work/agent.log" 2>/dev/null; do
	if ((SECONDS > deadline)); then
		echo "error: hot-swap never landed; agent log:" >&2
		cat "$work/agent.log" >&2
		exit 1
	fi
	sleep 0.2
done
echo "    hot-swap landed; agent still pid $agent_pid"

# --- 第二轮：v2 -----------------------------------------------------
echo "==> turn 2: asking again (v2 now serving)"
say "Roll the die once more with the dice tool, and report the JSON result."
wait_turn 8
roll2=$(grep '"role":"tool"' "$work/chat.jsonl" | tail -1)
echo "    tool result: $roll2"
echo "$roll2" | grep -qF '\"version\":\"v2\"' || {
	echo "error: turn 2 result is not v2" >&2
	exit 1
}

# --- 收尾 -----------------------------------------------------------
say "/quit"
wait "$agent_pid" 2>/dev/null || true
agent_pid=""
echo
echo "OK: same process, same session; tool results went v1 -> v2 across an in-place rebuild."
echo "    turn 1: $roll1"
echo "    turn 2: $roll2"
