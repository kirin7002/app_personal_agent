# H03 MySQL 任务、过期扫描与命令 Outbox

`vehicle/tasks` 实现车控任务持久化、10 分钟待回复到期控制、单轮单写幂等、命令 Outbox 与单调状态结果事件 Outbox，提供 `MySQLStore`（InnoDB + `FOR UPDATE` / `FOR UPDATE SKIP LOCKED`）与单进程模拟 `MemoryStore`。

## 数据库表（[tasks.sql](../vehicle/tasks/tasks.sql)）

- `vehicle_tasks`：任务主表，含 `scope_key`、`status`、`version`、`known_params_json`、`missing_fields_json`、`question`、`command_id`（唯一约束）、`expires_at` 及复合索引 `(scope_key, status, expires_at, created_at, task_id)`、`(status, expires_at)`。
- `vehicle_turn_writes`：按 `(scope_key, request_id)` 唯一记录每轮首次成功写入的规范化指纹与业务结果，实现同参重放返回原 ID、异参报 `IDEMPOTENCY_CONFLICT`。
- `vehicle_command_outbox`：与 `vehicle_tasks` 同事务写入的待发命令表，对 `task_id` 建唯一约束、`command_id` 为主键，支持 H05 基于租约互斥领取与崩溃恢复。
- `vehicle_result_events`：与终态更新同事务写入的独立结果事件表，按 `event_id = command_id + ":" + status` 去重，保留至少 24 小时。

## 共享接口归属

- **H02**：实现 `contract.PendingReader.ListPending`（严格 `expires_at > now`，`created_at DESC, task_id DESC` 至多 3 条，附 `Version` 与 `ParamsSchema`）及 `contract.ProgressReader.ReadTurnProgress`（无写入返回 `contract.ErrTurnHasNoWrite`）。
- **H04**：`MySQLStore` 写事务内支持 `TxLock` 回调（默认直接调用 `vehicle/turns.LockWritableTurn(ctx, tx, metadata, now)`），与 `CancelVehicleTurn` 锁同一 `voice_turns` 行，随后在同一事务内校验一次写幂等、候选版本、已知参数冲突并提交任务与 Outbox。
- **H05**：提供 `ClaimNextCommand`、`MarkCommandExecuting`、`ReleaseCommandForRetry`、`TransitionCommandOutcome` 与 `contract.ResultReader.ListVehicleResults`。
- **过期扫描**：`ExpiryWorker` 周期执行 `ExpirePending(ctx, now)`（`expires_at <= now`），候选查询与续办写入同步校验 `expires_at > now`，扫描滞后不可绕过精确 10 分钟边界。

## 验证命令

```bash
go test -race ./vehicle/tasks/... -v
go test -race ./...
go vet ./...
git diff --check
```
