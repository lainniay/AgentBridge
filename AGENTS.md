# AgentBridge — AGENTS.md

本文件定义 **AgentBridge** 的核心设计理念、系统架构、协议契约与工程规范，供人类开发者及参与本项目开发的所有 AI Agent（Antigravity、Codex 等）共同遵循。

---

## 1. 项目定位与核心原则

**AgentBridge** 是一个本地、对等（Peer-to-Peer）的多 Agent 协作桥接系统，首期连接 **Antigravity** 与 **Codex**，后续可扩展至其他具备原生 Harness 的 Coding Agent。

### 核心设计铁律（Invariants）

1. **原生 Harness 优先（Native Harness First）**
   - 不自建第三方模型壳子，让每个模型继续运行在各自效果最佳的原生 Harness 中（保留原生提示词、工具协议、上下文压缩、Prompt Cache、沙箱与交互体验）。
   - 仅在“高层用户意图与最终结论”层通过共享黑板与接力机制进行松耦合协作。
2. **抽象完全对等（Peer Symmetry）**
   - 在 Hub 核心中，Antigravity 与 Codex 地位完全平等，均为房间的 `Participant`，不预设主从关系。
   - 谁负责架构/人类沟通、谁负责编码执行、谁负责代码评审，完全由用户的实时工作流决定。
3. **按需绑定、不绑即透明（Zero-Overhead Opt-In）**
   - 未绑定任何协作房间时，所有 Agent 行为必须与普通独立 Agent 完全一致：
     - **零 Prompt 污染**：禁止在 always-on 规则中注入大段黑板使用说明；仅当 Hook 检测到当前会话已绑定房间时，才动态注入极简协作提示。
     - **零工作区磁盘副作用**：黑板与状态统一存放在 `${AGENTBRIDGE_HOME:-$HOME/.agentbridge}`，绝不在用户工作区创建 `.agent/blackboard/` 或修改任何 Git 文件。
     - **毫秒级快路径**：Hook 检测到未绑定应直接退出（`< 10ms`）并返回空对象 `{}`。
   - 支持随时 **中途挂载（Late Join，可选是否回填最近 N 轮正文）** 与随时 **解绑（Detach）**。
4. **后台自动采集，禁止 Agent 自写黑板**
   - 黑板内容由后台 Daemon 从两端原生持久化日志/Hook 中自动清洗并写入，Agent 仅通过只读工具读取。
   - 黑板**仅保留人类输入与 Agent 最终面向用户的正文**，剥离 `thinking`、完整 `tool_calls` 与中间过渡碎碎念；允许附带极轻量的 `files_touched` 元数据以防工作区状态失真。
5. **Turn 边界尾调用接力（Turn-Level Handoff，弃用嵌套同步 RPC）**
   - 跨 Agent 任务粒度通常较大，**不在一个 Turn 中途发起阻塞式同步子调用**（避免时间线倒挂、MCP 超时、游标空洞与互调死锁）。
   - 改为在当前 Turn 预约接力，待本轮**正常结束（`Stop`）后**由 Runtime 激活对端继续执行，保持全局黑板时间线严格扁平。

---

## 2. 系统三层架构

```text
   [Antigravity /codex Skill]              [Codex $agy Skill / CLI]
                    \                                    /
                     +------- (MCP / 本地 HTTP API) ------+
                                       |
                      +----------------v-----------------+
                      |      AgentBridge Hub (Go)        |
                      |  - Room & Binding 管理            |
                      |  - Blackboard 存储 (JSONL + MD)   |
                      |  - 对称 MCP Server (HTTP/Stdio)   |
                      |  - Turn-Level Handoff 调度器      |
                      +--------+----------------+--------+
                               |                |
                [Antigravity Adapter]      [Codex Adapter]
                  - 会话发现 (brain)         - 会话发现 (CODEX_HOME)
                  - 正文采集 (Hook/JSONL)    - 正文采集 (Watcher/Hook)
                  - 身份注入 & 未读提醒       - 身份路由 & 未读提醒
                  - 主动激活 (Phase 2)       - app-server 激活 (Phase 2)
```

### 层级职责边界

- **Control Plane（薄客户端）**：双端 Skill 通过对称 MCP 工具在原生会话内选择、绑定和解绑对端。CLI 通过本地 HTTP API 提供同类管理能力，包括 `link --latest` 快捷绑定。
- **AgentBridge Hub（Go Daemon，核心）**：**严格保持 Provider 无关**（`internal/hub` 与 `internal/blackboard` 中不得硬编码 Antigravity 或 Codex 专属逻辑）。
- **Agent Adapters（运行时适配器）**：吸收各宿主平台在日志格式、Hook 能力、身份注入与主动激活机制上的差异。

---

## 3. 黑板数据模型与游标语义

### 3.1 存储位置与工作区零侵入
- 所有状态与黑板均收口在 `${AGENTBRIDGE_HOME:-$HOME/.agentbridge}/`（兼容旧环境变量 `AGENTBRIDGE_STATE_DIR`），绝不写入用户项目工作区：
  - `state.json`：活跃 Room、Participant 与游标状态（全部成员解绑后自动清理空房间）。
  - `rooms/<room_id>.jsonl`：**唯一真值（Source of Truth）**，原子追加写入。
  - `rooms/<room_id>.md`：**可读视图**，供人类浏览与 `bridge_read(mode="path")` 返回。

### 3.2 JSONL 记录结构（单行）
```json
{
  "seq": 11,
  "ts": "2026-11-20T18:30:00+08:00",
  "room_id": "rm_01",
  "participant_id": "p_agy_1",
  "provider": "antigravity",
  "role": "assistant",
  "native_turn_id": "turn_abc",
  "content": "方案如下（同时作为给 Codex 的明确要求）：...",
  "files_touched": ["internal/auth/lock.go"],
  "handoff": {
    "target": "codex",
    "mode": "execute",
    "return_control": true
  }
}
```
- **幂等去重**：按 `(participant_id, native_turn_id, role)` 去重，防止 Watcher 重启或 Hook 重复触发导致重复写入。
- **Handoff 唤醒输入去重**：由 Runtime 通过接力主动发给对端的唤醒 Prompt，必须登记对应 `native_turn_id`，采集器看到该输入时跳过不写，防止将系统转发的上下文误标为人类 `<user>`。

### 3.3 Markdown 可读视图格式
```xml
<user seq="10" channel="antigravity">
帮我设计 auth 并发修复方案，然后交给 codex 去改，改完你帮我验收总结
</user>

<antigravity seq="11" handoff="codex" return_control="true">
方案与执行要求如下：...
</antigravity>

<codex seq="12" files_touched="internal/auth/lock.go">
已完成修改并通过测试：...
</codex>

<antigravity seq="13">
已验收 Codex 的修改，总结如下：...
</antigravity>
```

### 3.4 游标（Cursor）语义
- 因为时间线严格扁平，每个 `Participant` 只需维护一个**单调递增整数 `read_cursor`**。
- 当 Participant 调 `bridge_read(mode="diff")` 时：
  - 返回 `seq > self.read_cursor` 且**非本侧自己产生/已注入**的记录，并将 `self.read_cursor` 推进到最新 `seq`（当 `peek: true` 时不推进）。
- 当 Runtime 通过 Handoff 直接向对端推送增量上下文时，同步将该对端的 `read_cursor` 推进到已推送的最新 `seq`。

---

## 4. 对称 MCP 工具契约

所有接入的 Agent 看到完全一致的工具集（尽量克制 schema 体积）：

### Phase 1：被动共享黑板
1. **`bridge_status()`**
   - 返回当前会话绑定状态（未绑定时优雅返回 `{"bound": false}`）、所在房间成员列表、各对端未读条数及当前 Handoff 状态。
2. **`bridge_read(mode="diff"|"full"|"path", limit?, peek?, after_seq?, before_seq?)`**
   - `mode="diff"`（默认）：返回自上次读取/同步以来其他成员的新增正文，并推进游标（返回精简条目及 `room_id`、`cursor`、`latest_seq`、`has_more`）。
   - `mode="full"`：返回房间历史正文，支持 `limit` 搭配 `after_seq`（正向翻页）或 `before_seq`（逆向翻页）。
   - `mode="path"`：返回可读视图 `${AGENTBRIDGE_HOME:-$HOME/.agentbridge}/rooms/<room_id>.md` 的绝对路径。
3. **`bridge_manage(action, target_provider?, target_session_id?, backfill_turns=0, workspace?, caller_session_id?, request_id?)`**
   - `action="list"` 返回 `bound`、当前会话 `self`、当前房间及工作区内按最近活跃排序的对端候选。无法确定调用方时，`self` 和 `bound` 均为 `null`。候选提供 `provider`、`session_id`、`title`、`updated_at` 和 `already_bound`。
   - `action="bind"` 将已唯一识别的当前会话和指定目标绑定。目标可用具体 ID，也可用 `target_session_id="latest"` 选择最近活跃的未绑定对端。兼容已绑定一侧的 Late Join，并拒绝跨房间合并。零回填支持尚未落盘的 Antigravity 会话。
   - `action="unbind"` 解绑已唯一识别的当前会话。解绑一对一房间时一并解绑对端，停止两端采集。

### Phase 2：Turn-Level 接力（Handoff）
4. **`bridge_handoff(target, mode="consult"|"execute", return_control=false, note?)`**
   - **非阻塞预约**（立即返回，不等待对端执行）：向 Hub 登记“当前 Turn 正常结束后，将控制权接力给 `target`”。
   - Agent 应在当前给用户的正文回复中写清对 `target` 的要求、边界与验收标准（确保人类完全可见）。
   - 支持两种接力模式：
     - **`return_control: false`（单向接力）**：发起方结束 → Runtime 将增量正文（含要求）推给 `target` 激活执行 → `target` 完成后直接停下，控制权交还人类。
     - **`return_control: true`（闭环接力）**：适用于“请对端提意见/评审并由当前方汇总”或“委派编码并由当前方验收翻译成人话”的场景。发起方结束 → Runtime 激活 `target` → `target` 完成后，Runtime 自动再唤醒原发起方 1 次（推送 `target` 的结果）→ 原发起方总结汇报后停下，控制权交还人类。

### Handoff 护栏（强制要求）
- **仅正常结束才触发**：必须检查发起方的 `Stop` 终止原因（如 Antigravity 的 `terminationReason == "NO_TOOL_CALL"`）。若为 `USER_CANCELED`、`ERROR` 等异常终止，Runtime 必须自动作废本轮已预约的 Handoff。
- **自动接力跳数上限（`max_auto_hops`）**：Hub 维护连续自动接力计数器（默认上限 `2`，刚好覆盖一次完整的 `A → B → A` 闭环），每次人类发送新消息时归零；达到上限后禁止继续自动接力，强制交还人类。
- **工作区写互斥（Write Lease）**：同一 workspace 内若已有 `mode="execute"` 的接力任务在运行，拒绝并发触发第二个 `execute` 接力。
- **可选人工确认门禁**：控制面可切换“自动立即接力”或“接力前需用户确认”。

---

## 5. Adapter 能力与身份路由规范

### Antigravity Adapter
- **身份路由**：利用 Antigravity `hooks.json` 的 `PreToolUse` `overwrite` 能力，在直接调用 `bridge_*` 或通过 `call_mcp_tool` 转发时，将 stdin 中的 `conversationId` 注入参数。
- **未读提醒**：通过 `PreInvocation` Hook 检查是否有对端未读；仅在已绑定且有未读时注入极短 `systemMessage`（不强塞正文，保持按需拉取）。
- **采集时机**：`PreInvocation` 捕获用户输入（幂等写入），`Stop`（`NO_TOOL_CALL`）从 `transcriptPath` 提取本轮最终正文及 `files_touched`，并触发已预约的 Handoff。
- **Phase 2 反向唤醒**：通过宿主支持的本地通道唤醒已绑定的 Antigravity 会话。

### Codex Adapter
- **会话发现与采集**：扫描并监听 `${CODEX_HOME:-$HOME/.codex}/sessions/` 中匹配当前 workspace 的 JSONL，提取正文与文件变更。缺少原生标题时，以首条人类输入的摘要作为标题。
- **身份路由**：优先使用注入的 `_thread_id`、`_session_id` 或 MCP 连接的会话 ID。管理工具也接受从当前宿主取得的 `caller_session_id`。缺少明确 ID 时，可用每次调用独有的 UUID `request_id` 与当前 workspace 的近期在途 JSONL 记录精确关联。存在多个匹配或缺少匹配时，拒绝执行绑定和解绑。只读工具仍支持按 workspace 的唯一活跃绑定路由。
- **Phase 2 主动激活**：通过官方 `codex app-server`（双向 JSON-RPC 2.0：`thread/list`、`thread/resume`、`turn/start`）复用已有长会话，并将 `consult` 模式约束为只读沙箱，将审批请求转发至控制面。

---

## 6. 分阶段路线图

- **Phase 1 — 共享黑板（被动协同）**
  - Go Hub Daemon（Room 管理、JSONL + MD 黑板、游标、HTTP 控制 API、Streamable HTTP MCP）。
  - Antigravity & Codex 只读发现与正文采集 Adapter（含 Late Join / Detach 与零侵入退化）。
  - Antigravity `/codex` Skill 与 Codex `$agy` Skill 共用 `bridge_manage`，通过各自的原生对话管理绑定。
  - 验证闭环：用户可在两窗口自由切换聊天，通过 `bridge_read` 零回声同步增量上下文。

- **Phase 2 — Turn-Level Handoff（主动接力）**
  - Hub 接力状态机（`bridge_handoff`、`return_control: false | true`、异常作废、`max_auto_hops`、写锁）。
  - Codex `app-server` Dispatch Adapter 与控制面审批请求转发。
  - Antigravity 反向唤醒 Adapter，跑通 `Agy → Codex → Agy` 闭环接力与 `Codex → Agy → Codex` 反向咨询。

---

## 7. 推荐目录结构与开发规范

```text
AgentBridge/
├── AGENTS.md
├── go.mod
├── cmd/
│   └── agentbridge/          # Daemon 启动入口 & CLI 命令 (install/uninstall/doctor/link/status/unlink)
├── internal/
│   ├── hub/                  # Room、Participant、Binding、Handoff 状态机（Provider 无关）
│   ├── blackboard/           # JSONL 原子写入、幂等去重、MD 渲染（存放于 AGENTBRIDGE_HOME/rooms）
│   ├── mcp/                  # 对称 MCP Server (Streamable HTTP + stdio 代理)
│   ├── api/                  # 控制面本地 HTTP API 和 MCP 会话管理
│   └── adapter/
│       ├── adapter.go        # 统一 AgentAdapter 接口定义
│       ├── antigravity/      # Antigravity transcript 解析、Hook 脚本协同与唤醒实现
│       └── codex/            # Codex sessions JSONL 解析、Hook 协同与 app-server 客户端
└── plugins/
    ├── antigravity/          # Antigravity 插件及 /codex Skill
    └── codex/skills/agy/     # Codex $agy Skill
```

### 编码与测试要求
- **模块解耦**：`internal/hub`、`internal/blackboard` 与 `internal/mcp` 严禁 import `internal/adapter/*`。MCP 通过管理接口调用控制面。
- **并发与持久化安全**：Room 状态、游标更新与 JSONL 写入必须具备并发保护（锁 + 原子文件替换/追加），避免多请求交错写坏文件。
- **Hook 脚本性能与健壮性**：所有 Hook 命令必须设置短超时、严格处理 JSON 编解码错误；任何 Hub 不可达的异常情况均应**降级放行（fail-open）**，绝不能阻塞用户正常的 Agent 对话。
- **测试覆盖**：
  - 使用脱敏后的真实 JSONL fixture 覆盖 Antigravity 与 Codex 的正文提取、tool call 剥离与 `files_touched` 解析；
  - 重点单测游标推进、Late Join 回填、Handoff 状态机（`return_control: false/true`、`USER_CANCELED` 取消、`max_auto_hops` 截断、重复唤醒去重）。
