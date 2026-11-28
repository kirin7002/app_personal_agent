# H07 两轮关窗联调

目标路径为「把车窗关一下」→ 车窗位置追问 →「左后窗」→ 模拟车辆执行确认 → 车端完成播报。两轮各使用一个 Hermes Run，并续办同一个任务。完成语音由车辆结果事件触发。

## 复现

开发联调与故障验证。

```sh
go test -race ./... -count=1
go vet ./...
git diff --check
```

真实百炼音频、真实 Hermes、MySQL 和模拟车辆联调需要以下配置。

| 环境变量 | 内容 |
| --- | --- |
| `H07_LIVE` | 设置为 `1` 启用真实服务测试 |
| `DASHSCOPE_API_KEY` | 由本机密钥配置提供的百炼 API Key |
| `DASHSCOPE_REGION` / `DASHSCOPE_WORKSPACE_ID` | 百炼地域及业务空间，用于专属域名。省略 Workspace ID 时，北京和新加坡使用对应的区域域名 |
| `H07_HERMES_PYTHON` | H01 锁定 Hermes 部署的 Python 可执行文件，包含固定 MCP SDK |
| `H07_HERMES_AGENT_DIR` | Hermes Agent 源码目录，Git 提交前缀为 `72c88037` |
| `H07_MYSQL_DSN` | 可选，独立 MySQL 测试数据库（具备建表和读写权限）；留空时 `TestLiveScenarioA` 自动使用 SQLite（`H07_SQLITE_PATH` 或临时文件） |
| `H07_SQLITE_PATH` | 可选，指定 SQLite 数据库文件路径替代 MySQL（默认使用测试临时目录） |
| `H07_PCM_DIR` | 两个输入样本 `close-window.pcm` 和 `rear-left.pcm` 的目录 |
| `H07_ARTIFACT_DIR` | 可选，用于保存追问和确认播报的 24 kHz WAV |

真实测试启动临时 Hermes Home、独立 HTTP MCP 服务和车端 WebSocket。车端播报使用 macOS 本地普通话 TTS。可以将下面的合成样本替换为真人录音生成的单声道 16 kHz PCM16LE。

```sh
./scripts/car-audio-samples.sh /tmp/app-personal-agent-car-samples
export H07_PCM_DIR=/tmp/app-personal-agent-car-samples
export H07_ARTIFACT_DIR=/tmp/app-personal-agent-voice-evidence
H07_LIVE=1 go test -race ./integration -run '^TestLiveScenarioA$|^TestMySQL' -count=1 -v
```

## 百炼独立实测

北京区域域名，默认 `qwen-audio-3.1-realtime-flash`，使用 macOS 普通话合成样本。第一次探针的 `session.updated` 为连接后 1.119 秒，关窗和闲聊各自从提交到最终转写为 479 与 425 毫秒，分别识别为「把车窗关一下。」与「你好。」。两条最终转写抵达后才发送 `response.create`，981 毫秒后收到该响应首段音频。本次共收到 22 段、407,040 字节 PCM，转换为同规格 WAV 后 `afinfo` 验证是单声道 24 kHz、16 位、8.48 秒。第二次探针首段音频后发送取消，收到了 `response.done` 的 `cancelled`。阶段耗时为本次观测。百炼区域域名实测采用 [官方 WebSocket 接入说明](https://help.aliyun.com/zh/model-studio/audio-realtime-websocket-access) 中继续支持的地址。

## 最终验收结果

2026-11-27 在双方模块均合入的 `origin/main` 基线 `3016789` 上执行。真实 Qwen、Hermes `72c88037`、MySQL `8.4.13` 与 H05 模拟车辆的两轮验收通过，单次测试 17.37 秒。语音输入使用普通话合成样本，车端通过 WebSocket 模拟。

| 步骤 | 实测证据 |
| --- | --- |
| 第一轮最终转写和追问 | 「把车窗关一下。」→ `1ff1bd11…` / `voice-1ff1bd11…` / `run_080a2884…` → `create_vehicle_task`，`TD2144288E7FA6A34` 进入 `pending_reply`。追问「要关哪扇车窗？」以 `reply-1ff1bd11…` 播报，83,202 字节。 |
| 第二轮最终转写和续办 | 「左后窗。」→ `10dfa6a1…` / `voice-10dfa6a1…` / `run_40855935…` → `dispatch_vehicle_command`，沿用原任务，参数为 `position=rear_left`，生成 `CA18B3A494EE99F4C`。 |
| 受理与执行确认 | 模拟通道只受理时记录 `VEH-0001`、状态 `executing`，结果事件数为 0。相同完成回执送达两次，命令发送次数为 1，结果事件数为 1，事件 `CA18B3A494EE99F4C:completed`。 |
| 确认播报 | `result-8b0a4d85…` 播报「车窗已关闭。」，63,376 字节。两段 WAV 通过 `afinfo` 检查，分别为 1.733 秒和 1.320 秒的 24 kHz 单声道 16 位 PCM。 |
| 可信轮次 | 实际 MCP 工具回调的两个 HTTP 绑定头与 MCP `params._meta` 一致，写工具按创建、续办顺序各调用一次。凭据与句柄不进入证据。 |

预联调时，第二轮短音频曾转写成「做后窗。」。H04 返回澄清并保留待回复任务，命令 Outbox 未写入。Qwen 会话加入等权重的四个位置热词后，独立探针和最终联调均转写为「左后窗。」。采用普通热词权重 5，Go 继续校验供应商返回的原始最终转写。[百炼客户端事件](https://help.aliyun.com/zh/model-studio/fun-audiochat-client-events)

## 故障与边界覆盖

| 验证 | 结果 |
| --- | --- |
| `TestSimulatedScenarioA` | 模拟 Qwen、模拟 Hermes 使用真实 Go MCP 和 H05 播报路径通过。 |
| `TestMySQLTurnCancelFence` / `TestMySQLPendingCandidateBoundary` | 真实 MySQL 下取消等待写锁、取消后拒绝旧句柄、最新三条候选、同参重放和精确十分钟到期通过。 |
| `vehicle/runs` 与 `vehicle/turns` | 受理重试、SSE 断流、重复序号、提前终态、并行重放和 202 前回调通过。 |
| `vehicle/mcp`、`vehicle/tasks` 与 `vehicle/executor` | 双候选匹配、否定和只读隔离、跨作用域拒绝、部分补参、命令租约与崩溃恢复、过期车况、不确定结果及迟到回执通过。 |
| `voice/gateway` 与 `realtime/qwen` | 原生出声、转写分片隔离、打断、重连、旧代数和重复帧、配置超时、1002 和结果抢先到达通过。 |

真实车辆通道、车机麦克风和扬声器、业务空间专属域名、真实样本规模下的 P50/P95 仍需对应环境验证。


## 恢复路径补验

- 使用模拟通道注入发送成功后、保存通道响应前的 worker 中断。新 worker 在租约过期后查询原命令，通道只收到一次发送。明确可重试的失败仍沿用原指令 ID，完成事件只生成一次。
- 独立 SQLite 连接按队列顺序处理同一会话和车辆。另一个车辆作用域可以独立进入，过期票据可清理。
- 已确认的车辆结果音频通过数据库保留播放回执。新的网关实例仍能交付业务结果，音频保持去重。模拟车端收到重复稳定 ID 时复用本地文件。
- 真实百炼与 Hermes 的两轮关窗在 MySQL 8.4.13 和 SQLite 上均通过，包含执行回执去重和播放回执落库。真人录音、真实车机和车辆通道仍依赖对应环境。

## 延迟基线

复用普通话合成样本，使用北京区域域名。冷连接表示新建 Qwen WebSocket 后的第一轮，热连接表示复用同一连接的第二轮。下表测量车端发出说完事件到收到首帧音频的时间。车控使用 SQLite、真实 Hermes 和本机 TTS，统计的是追问或处理中播报。每组 5 次，P95 使用最近秩法。本表记录当前测试环境的小样本基线。

| 路径 | 连接 | P50 | P95 |
| --- | --- | ---: | ---: |
| 闲聊 | 冷 | 1.475 秒 | 1.622 秒 |
| 闲聊 | 热 | 1.420 秒 | 1.630 秒 |
| 车控 | 冷 | 6.827 秒 | 8.003 秒 |
| 车控 | 热 | 5.276 秒 | 5.532 秒 |

连接就绪时间的 P50/P95 分别为闲聊 1.190/1.588 秒、车控 1.128/1.971 秒。可按上文配置生成三段 PCM，再读取测试输出中的 `latency` 行。

```sh
H07_LIVE=1 go test -race ./integration -run '^TestLiveScenarioA$|^TestLiveChatLatency$' -count=5 -v
```
