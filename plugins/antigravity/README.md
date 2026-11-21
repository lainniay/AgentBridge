# 安装与运行 AgentBridge

AgentBridge 将双端插件与 Skill 内嵌在单个 Go 二进制中，一条命令即可完成安装或升级（尊重 `AGENTBRIDGE_HOME` 与 `CODEX_HOME` 环境变量）：

```sh
# 方式 A：通过 go install 安装
go install github.com/lainniay/AgentBridge/cmd/agentbridge@latest
agentbridge install

# 方式 B：从源码仓库直接构建并安装
go build -o ./agentbridge ./cmd/agentbridge
./agentbridge install
```

`install` 会自动完成：
1. 将二进制安装到 `~/.gemini/config/plugins/agentbridge/bin/agentbridge`；
2. 安装 Antigravity 插件（`plugin.json`、`hooks.json`、`mcp_config.json`、`/codex` Skill），并清理旧版 `sidecars/`；
3. 安装 Codex `$agy` Skill 到 `${CODEX_HOME:-$HOME/.codex}/skills/agy/SKILL.md`；
4. 若本机 `codex` CLI 可用，自动注册 `agentbridge` MCP（使用 `mcp-stdio --provider codex --workspace .`，全局一次配置、所有仓库通用）。

## 启动 Daemon 与自检

Daemon 由你自行启动或交由自己的进程管理工具（如 `launchd` / `tmux`）托管：

```sh
"$HOME/.gemini/config/plugins/agentbridge/bin/agentbridge" daemon
```

运行自检命令确认插件、Skill、`CODEX_HOME`、`AGENTBRIDGE_HOME` 和 Daemon 监听状态：

```sh
"$HOME/.gemini/config/plugins/agentbridge/bin/agentbridge" doctor
```

数据默认存放于 `${AGENTBRIDGE_HOME:-$HOME/.agentbridge}/`（含 `state.json` 与 `rooms/<room_id>.{jsonl,md}`），不会在代码工作区写入任何文件。

## 常用 CLI 命令

```sh
bridge="$HOME/.gemini/config/plugins/agentbridge/bin/agentbridge"
"$bridge" link --latest --workspace "$PWD"   # 终端一键绑定当前目录最近活跃的一对会话
"$bridge" status --workspace "$PWD"          # 查看当前工作区活跃房间
"$bridge" uninstall                          # 卸载双端插件与 Skill（加 --purge 同时清理数据目录）
```
