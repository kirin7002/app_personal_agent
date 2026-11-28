# H05 异步执行器、车辆回执、对账与完成事件

`vehicle/executor` 实现 H03 命令 Outbox 的租约领取、下发前能力与前置条件重校验、模拟车辆通道、回执消费、车况对账以及单调结果事件发布。

## 下游车辆通道契约（首期模拟通道与真实通道基线）

| 能力项 | 模拟通道（首期默认） | 真实车辆通道接入要求 |
| --- | --- | --- |
| 关联键 | Go 分配 `command_id`，通道映射 `downstream_id` | 必须回传 `command_id` 或由适配器持久化 `downstream_id ↔ command_id` 映射 |
| 受理与执行区分 | `OutcomeAccepted` 仅推进 `executing`，不产生完成事件 | 受理回执仅推进 `executing`；终态必须来自执行回执或下发后新鲜车况 |
| 下游幂等 | `SupportsIdempotency=true`，重试沿用同一 `command_id` | 若不支持或能力未知，发送结果不明时禁止自动重发，必须走对账或 `result_unknown` |
| 重试窗口 | `RetryWindow=2m`（仅限可重试发送失败） | 按真实通道限流与超时窗口配置 |
| 确认超时 | `ConfirmTimeout=30s`（超时未确认转 `result_unknown`） | 按车型指令平均执行耗时配置 |
| 对账窗口 | `ReconcileWindow=5m`（窗口内迟到终态可推进，超窗记录 `LateEvidence`） | 与既有车控异常处理流程对接 |
| 车况确认 | 复用 H06 `ConfirmCommandEffect`（目标车、目标值、下发后采样、`fresh`；空调同时校验 `power=on` 与目标设温） | 需保证车端信号采样时钟单调且可判定新鲜度 |

## 发送与恢复

worker 验证当前租约和尝试编号后，先在数据库中登记 `executing` 和发送时间，再调用车辆通道。发送后在保存回执前中断时，接手的 worker 根据 `command_id` 查询或等回执。可证实失败且通道支持幂等时，重试保留原 `command_id`。只有明确执行回执或满足时序的目标车况才产生 `completed` 事件。

升级时先停止旧 worker，并核对旧版已领取、尚处于 `pending_dispatch` 的指令。旧版在发送后才写入执行状态，这些记录需要用原 `command_id` 对账后再恢复处理。

## 通知与静态反馈

- 终态（`completed`、`failed`、`result_unknown`）更新与 `vehicle_result_events` 写入在 H03 同一事务内原子提交，按 `event_id = command_id + ":" + status` 去重，保留至少 24 小时，供 H08 通过 `contract.ResultReader` 消费与断线重取。
- `FeedbackText` 提供静态中文语音反馈文本，确保只有 `completed` 才会播报完成。

## 验证命令

```bash
go test -race ./vehicle/executor/... -v
go test -race ./...
go vet ./...
git diff --check
```
