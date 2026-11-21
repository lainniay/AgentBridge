---
name: codex
description: 通过 /codex 在当前 Antigravity 会话中选择、绑定或解绑 Codex 会话。
---

# 连接 Codex 会话

当前会话身份与工作区由 Hook 自动注入，无需手动查找自己的 ID。

- **`/codex`**：调用 `bridge_manage(action="list", target_provider="codex")`。
  - 无候选时直接说明当前工作区没有可选 Codex 会话。
  - 有候选时用原生 `ask_question` 展示前 5 个（格式：`标题 · 相对活跃时间`，已绑定加 `[已绑定]`，首个可选标 `(Recommended)`）；若当前会话已绑定，末尾加 `解绑当前会话`。
  - 选中会话后调用 `bridge_manage(action="bind", target_provider="codex", target_session_id=<ID>)`。
- **`/codex latest`**：直接调用 `bridge_manage(action="bind", target_provider="codex", target_session_id="latest")`。
- **`/codex unlink`**（或选“解绑当前会话”）：调用 `bridge_manage(action="unbind")`。

可选参数：用户要求回填历史时传 `backfill_turns`（默认 `0`）；指定目录时在 `list` 与后续调用中传同一 `workspace`。绑定后可用 `bridge_read` 读取对端增量。
