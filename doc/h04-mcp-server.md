# H04 Go MCP 服务与写入判定

`vehicle/mcp` 提供面向 Hermes 的 Streamable HTTP MCP 服务端，完整实现 `initialize`、`ping`、`tools/list`、`tools/call` 与六个车辆工具适配器，并将服务鉴权、可信轮次校验、取消写锁和 Go 侧写入判定收敛在 MCP 边界。

## 安全与写入边界

- **服务与传输鉴权**：强制校验 `Authorization: Bearer <ServiceToken>`（常时间比对），从受信任 HTTP 头 `X-Vehicle-Turn-Handle`、`X-Vehicle-Hermes-Session-ID` 解析 `contract.TurnMetadata`；若 MCP `params._meta` 携带 `vehicle.turn_handle` / `vehicle.hermes_session_id`（或启用 `RequireMeta`），强制与 HTTP 头完全一致。拒绝模型在 `arguments` 中夹带任何身份/轮次字段。
- **读/写分离绑定**：
  - 四个只读工具（`list_vehicle_tasks`、`get_vehicle_status`、`search_vehicle_manual`、`search_vehicle_capabilities`，均标注 `readOnlyHint: true`）通过 `contract.TurnRegistry.ResolveTurn` 验证轮次并直连 H03/H06。
  - 两个写工具（`create_vehicle_task`、`dispatch_vehicle_command`，标注 `readOnlyHint: false`）在 MySQL 写事务内通过 `vehicle/turns.LockWritableTurn` 锁定与 `CancelVehicleTurn` 同一行，与单轮单写幂等、候选版本校验及任务/命令 Outbox 写入原子提交。
- **Go 写入判定（`guard.go`）**：
  - 拒绝只读查询、否定/取消语句产生写入。
  - 短回复省略 `task_id` 不得降级为新任务或新指令（返回 `CLARIFICATION_REQUIRED`）。
  - 多条待办均匹配且原话未消歧时返回 `CLARIFICATION_REQUIRED`；提交参数与原话矛盾时拒绝写入。
  - 校验 `task_id` 必须属于本轮候选快照并传递快照 `Version` 给 H03；同轮同参重放从 H03 幂等记录恢复原结果。

## 验证命令

```bash
go test -race ./vehicle/mcp/... -v
go test -race ./...
go vet ./...
git diff --check
```
