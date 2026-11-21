# AgentBridge

本地、对等（Peer-to-Peer）的多 Agent 协作桥接系统，连接 **Antigravity** 与 **Codex** 的原生会话。

- **原生 Harness 优先**：每个模型继续运行在各自效果最佳的原生 Harness 中，只在高层对话意图与结论层共享黑板。
- **按需绑定、不绑即透明**：未绑定时零上下文污染、零工作区文件侵入；所有黑板与状态存放在 `${AGENTBRIDGE_HOME:-$HOME/.agentbridge}/`。
- **零第三方依赖单二进制**：内置 Antigravity 插件与 Codex Skill，一条命令完成安装。

## 快速安装

```sh
go install github.com/lainniay/AgentBridge/cmd/agentbridge@latest
agentbridge install
```

或从源码目录构建安装：

```sh
go build -o ./agentbridge ./cmd/agentbridge
./agentbridge install
```

`agentbridge install` 会自动：
1. 安装二进制与 Antigravity 插件（`plugin.json`、`hooks.json`、`mcp_config.json`、`/codex` Skill）到 `~/.gemini/config/plugins/agentbridge/`；
2. 安装 Codex `$agy` Skill 到 `${CODEX_HOME:-$HOME/.codex}/skills/agy/SKILL.md`；
3. 若本机 `codex` CLI 可用，自动通过 `codex mcp add` 注册 `agentbridge` MCP。

## 启动 Daemon 与自检

Daemon 由你自行启动或交由个人进程管理工具（如 `launchd` / `tmux`）托管：

```sh
"$HOME/.gemini/config/plugins/agentbridge/bin/agentbridge" daemon
```

检查配置与监听状态：

```sh
"$HOME/.gemini/config/plugins/agentbridge/bin/agentbridge" doctor
```

## 日常使用

- **在 Antigravity 中**：输入 `/codex`（选择当前工作区的 Codex 会话绑定）、`/codex latest` 或 `/codex unlink`。
- **在 Codex 中**：输入 `$agy`（选择当前工作区的 Antigravity 会话绑定）、`$agy latest` 或 `$agy unlink`。
- **同步增量**：两端均通过 `bridge_read` 读取对端新增正文（支持 `mode="diff"|"full"|"path"` 与 `limit`/`after_seq`/`before_seq` 翻页），通过 `bridge_status` 查看绑定与未读状态。
- **终端快捷命令**：
  ```sh
  agentbridge link --latest --workspace "$PWD"
  agentbridge status --workspace "$PWD"
  agentbridge uninstall        # 加 --purge 同时清理 AGENTBRIDGE_HOME
  ```

## 架构规范

详见 [AGENTS.md](./AGENTS.md)。
