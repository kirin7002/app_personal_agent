# H06 功能地图与只读服务

`vehicle/catalog` 提供版本化车辆功能地图、车型约束适配、BM25 能力召回、单一真相参数 Schema 校验、车况信号读取（含 H05 复用的执行确认逻辑）以及按车型与版本隔离的车书检索。

## 模块与契约

- `catalog.Service`：
  - 注册核心操作：`window.close`、`window.open`、`window.get_status`、`hvac.set_power`（保留设温）、`hvac.set_temperature`、`hvac.set_climate`、`hvac.get_status`。
  - 同一份 `OperationSpec` 同时输出 `contract.Capability.ParamsSchema` 与运行时 `ValidateMutation` 校验，支持按车型动态裁剪温度范围与禁用能力。
  - 实现 `contract.CapabilityFinder`（BM25 中英文 unigram/bigram + ASCII 分词），首轮零召回不限制后续 `SearchCapabilities` 补搜。
  - 提供 `GetVehicleStatus` 与 `AllWindowsClosed`：逐信号返回 `value`、`sampled_at` 与 `fresh`/`stale`/`unavailable`，任一车窗过期或不可用时不推断全部关闭。
  - 提供 `ConfirmCommandEffect` 供 H05 复用：校验目标车辆、目标值（空调同时要求 `power=on` 与目标温度）、`sampled_at >= dispatchedAt` 与新鲜度。
  - 提供 `SearchManual`：按车型和车书版本过滤，支持 `topic_id` 精确命中与 BM25 检索，每条结果携带 `version` 与可追溯 `source`。

## 验证命令

```bash
go test -race ./vehicle/catalog/... -v
go test -race ./...
go vet ./...
git diff --check
```
