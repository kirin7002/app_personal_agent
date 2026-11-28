# H00 语音车控共享契约

Go 模块为 `github.com/kirin7002/app_personal_agent`。该仓库的初始基线包含设计和实施计划。车端客户端、TTS、数据库迁移、车辆事件通道由任务卡引入。Go 契约在 [contract](../../contract)，A/B 和错误报文在 [testdata](../../contract/testdata)。

| 边界 | 提供方 → 消费方 | 契约 |
| --- | --- | --- |
| 最终转写 | H09 → H08 → H02 | `FinalTranscript` 只在供应商完成转写时产生。H08 根据连接代数和输入项关联到 `VehicleTurn`。认证入口绑定 `Scope`。 |
| 待回复候选 | H03 + H06 → H02 / H08 | `PendingReader` 根据同一用户、业务会话和车辆读取 `pending_reply`，严格按 `expires_at > now` 过滤，再按 `created_at DESC, task_id DESC` 返回至多三条。每条附版本和参数 Schema。 |
| 可信轮次 | H02 → H01 → H04 | H02 先持久化 `TurnContext` 和候选版本，H01 通过认证的服务链路传播 `TurnMetadata`，H04 调用 `TurnRegistry` 并在写事务内重校验轮次。 |
| 写入结果 | H03 / H04 → H02 | `ProgressReader` 根据可信作用域与 `request_id` 读本轮实际状态和追问。H02 按该状态批准播报。 |
| 车辆结果 | H05 → H08 | `ResultReader` 返回保留期内的独立事件，H08 按完整 `Scope` 路由并按 `event_id` 去重。 |
| 工具 | H04 → H01 | 六个原始工具名和 Go 输入输出见 [tools.go](../../contract/tools.go)。错误结果采用 MCP `isError=true`，内容为 `BusinessError`，同一 JSON 可置于 `structuredContent` 与兼容文本内容。 |
| 车辆指令 | H03 → H05 | Go 在登记 outbox 的事务内分配唯一 `command_id`。同一命令重试复用该 ID。如通道返回自身 ID，H05 持久化其与 `command_id` 的关联。 |

## 轮次与车辆状态

- 认证服务解析用户、会话和所选车辆。Go 为每个作用域映射一个稳定 Hermes API 会话，启动每轮 Run 前预登记随机句柄。HTTP 服务间传输使用 `X-Vehicle-Turn-Handle` 和 `X-Vehicle-Hermes-Session-ID`，H01 负责请求级传播和服务鉴权。句柄留在服务端传输和存储，模型只看最终原话、候选问题、已知参数、缺失字段及 Schema。
- H02 取消轮次和 H04 提交写入在同一持久化边界按轮次串行化。取消先完成时拒绝写入，写入先完成时保留车辆任务。每轮最多提交一次业务写入。同参数重放返回之前的结果，参数变化报 `IDEMPOTENCY_CONFLICT`。
- 部分补参通过带 `task_id` 的 `create_vehicle_task` 保存，要求版本匹配，保留原截止时间。补齐后由 `dispatch_vehicle_command` 注册指令。受理只推进到执行中。只有可信执行证据能产生 `completed`。
- H05 在状态更新的事务内记录事件，`event_id` 对每个命令和结果类型稳定。执行结果事件类型为 `completed`、`failed`、`result_unknown`。结果事件首期保留至少 24 小时，支持认证车端重连后读取这一窗口。待回复时限为创建后 10 分钟。
- 模拟通道覆盖关窗、开窗、空调及车况联调。真实车辆支持矩阵待 H05/H07 确认，首期运行配置使用模拟通道。

## 车端 WebSocket

连接后先发 `session.open`，携带 `session_id`、所选 `vehicle_id` 和 `input_format`。服务端从握手 Bearer 解析作用域，收到百炼配置确认后回 `session.ready`，携带 `connection_generation` 和输入输出格式。输入为单声道 PCM16LE/16 kHz，输出为单声道 PCM16LE/24 kHz。音频分片为 JSON 中的 Base64 字段。

- 客户端用同一 `voice_turn_id` 发 `speech.started`、一组从 1 递增的 `audio.input.sequence`，以及 `speech.ended`。当前音频连接代数随这三类帧传入。客户端 ID 在同一作用域内唯一。网关生成业务轮次和稳定 `request_id`。
- 服务端发送 `speech.reply` 或独立 `vehicle.result`，音频由 `playback.start`、`audio.output`、`playback.stop` 成组交付。帧携带播放 ID 和代数，音频从 1 起连续编号。车端起讲时立即停止本地旧播放。
- 车端完整消费一次播报后发 `playback.ack`，带播放 ID 和代数。结果播报的 ID 由 `event_id` 稳定派生。重连时车端使用已消费 ID 去重。被用户打断的语音在本业务会话内停止自动续播，业务结果可重取。
- 百炼重连产生新的连接代数。收到新的 `session.ready` 后重新采集不完整音频。轮次、任务、命令按各自的稳定标识关联。

## 验证

运行 `go test -race ./contract` 检查 A/B 的最终转写、待回复顺序、工具写入结果、独立车辆事件及失败报文。使用 `go vet ./...` 和 `git diff --check` 检查 Go 静态问题和补丁格式。
