---
name: agy
description: 通过 $agy 在当前 Codex 会话中选择、绑定或解绑 Antigravity 会话。
---

# 连接 Antigravity 会话

## 1. 携带当前会话身份（每次调用 `bridge_manage`）
- 优先从环境读取：`printf '%s\n' "${CODEX_THREAD_ID:-${CODEX_SESSION_ID:-}}"`；非空时传入 `caller_session_id`（严禁把对端 ID 当作 `caller_session_id`）。
- 若环境为空且 MCP 未配置 `--session-id`，每次调用用 `uuidgen` 生成新 UUID 作为 `request_id`（桌面 `functions.exec` 需以 `const result = await tools.mcp__agentbridge__bridge_manage({...})` 开头并内联双引号 JSON 字面量）。

## 2. 执行操作
- **`$agy`**（或自然语言连接 Antigravity）：调用 `bridge_manage(action="list", target_provider="antigravity")`，按编号列出当前工作区会话（标题、相对活跃时间、绑定状态）让用户回复编号；选中后调用 `bridge_manage(action="bind", target_provider="antigravity", target_session_id=<ID>)`。
- **`$agy latest`**：直接调用 `bridge_manage(action="bind", target_provider="antigravity", target_session_id="latest")`。
- **`$agy unlink`**：调用 `bridge_manage(action="unbind")`。

可选参数：需要回填历史时传 `backfill_turns`（默认 `0`，等待落盘的新会话用 `0`）；指定目录时传同一 `workspace`。返回 `bound: null` 表示未能识别当前会话。绑定后可用 `bridge_read` 读取对端增量。
