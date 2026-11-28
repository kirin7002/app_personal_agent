# 业务 MCP 工具清单

字段类型、必填项和完整 JSON 见 [MCP 工具 Schema](./MCP%20工具%20Schema.md)。

本清单以 `feature_v2.9` 分支的 [业务图入口 `appai.fast_graph`](../appai/fast_graph.py#L240) 为范围。2026-11-27 使用 `RUNENV=dev` 查询了三个 MCP 服务的工具列表，再按当时的 `disable_tools` 配置和 Agent 注册规则核对。其他环境的工具列表和配置可能不同。

## 工具数量

| MCP 服务 | 环境配置键 | 服务端返回 | 按当前规则注册 |
| --- | --- | ---: | ---: |
| 基础车控 `remote` | `car_ctrl_base_sse` | 24 | 21（基础车控 19，预约车控 2） |
| 车况 `car_status` | `car_status_sse` | 5 | 4 |
| 备车 `complex-ctrl` | `car_ctrl_complex_sse` | 2 | 1 |
| **合计** | | **31** | **26** |

## 车控 MCP

| 工具 | 用途 | dev 注册状态 |
| --- | --- | --- |
| `ac_ctrl` | 空调控制 | 基础车控 |
| `seat_heat_ctrl` | 座椅加热 | 基础车控 |
| `seat_vent_ctrl` | 座椅通风 | 基础车控 |
| `row23_mode_ctrl` | 后排座椅模式 | 基础车控 |
| `car_lock_ctrl` | 车辆解锁、闭锁 | 基础车控 |
| `car_window_ctrl` | 车窗控制 | 基础车控 |
| `sliding_door_ctrl` | 侧滑门控制 | 基础车控 |
| `front_trunk_ctrl` | 前备箱控制 | 基础车控 |
| `trunk_ctrl` | 后备箱控制 | 基础车控 |
| `sunroof_ctrl` | 天窗控制 | 基础车控 |
| `sunshade_ctrl` | 遮阳帘控制 | 基础车控 |
| `steer_wheel_heat_ctrl` | 方向盘加热 | 基础车控 |
| `mirror_heat_ctrl` | 后视镜加热 | 基础车控 |
| `bat_heat_ctrl` | 电池加热 | 基础车控 |
| `charge_start_stop_ctrl` | 启动或停止充电 | 基础车控 |
| `charger_unlock_ctrl` | 充电枪解锁 | 基础车控 |
| `healthy_charge_ctrl` | 健康充电控制 | 基础车控 |
| `horn_honk_ctrl` | 鸣笛寻车 | 基础车控 |
| `navi_ctrl` | 车机导航 | 基础车控 |
| `timed_charge_ctrl` | 预约充电 | 预约车控 |
| `timed_bat_heat_ctrl` | 预约电池加热 | 预约车控 |
| `ac_appoint_ctrl` | 预约空调 | 配置禁用 |
| `child_seat_vent_ctrl` | 儿童座椅通风 | 配置禁用 |
| `deeplink_ctrl` | 页面跳转 | 配置禁用 |

基础车控按配置 `disable_tools` 过滤服务端工具，预约车控用独立白名单保留 `timed_charge_ctrl` 和 `timed_bat_heat_ctrl`。[基础车控注册](../appai/car_ctrl/ctrl_agent_node_graph.py#L45) · [预约车控注册](../appai/car_ctrl/appoint_ctrl_agent_node.py#L40) · [默认禁用配置](../nacos/app_ai_agent.yaml#L238)

## 车况 MCP

| 工具 | 用途 | dev 注册状态 |
| --- | --- | --- |
| `car_airconditioner_status` | 查询车内温度、空调开关、设定温度等 | 车况 Agent 封装注册 |
| `car_appointment_record` | 查询预约备车、预约空调、预约电池预热 | 车况 Agent 封装注册 |
| `car_charging_info` | 查询充电上限、预约充电开关和时间 | 车况 Agent 封装注册 |
| `car_permission_info` | 查询 GPS 权限及 AI 停车位、停车照片等 | 车况 Agent 封装注册 |
| `car_signal_status` | 查询车辆信号状态 | 服务端提供，车况 Agent 使用本地 `query_car_status` 查询实时车况 |

车况 Agent 同时绑定 3 个本地工具，因此共绑定 7 个工具。查询是否正在充电、剩余充电时间走 `query_car_status`，查询充电上限与预约充电设置走 `car_charging_info`。[车况工具注册](../appai/car_status/car_status_agent_node.py#L510) · [实时信号查询](../appai/car_status/car_status_agent_node.py#L342)

## 备车 MCP

| 工具 | 用途 | dev 注册状态 |
| --- | --- | --- |
| `vehicle_prep_ctrl` | 生成预约备车指令 | 预约备车 Agent |
| `car_prepare_ctrl` | 备车控制 | 预约备车 Agent 显式过滤 |

预约备车先使用本地工具 `resolve_time_tool` 解析时间，得到有效时间后再调用 `vehicle_prep_ctrl`。[备车工具注册](../appai/car_prepare/prepare_agent_factory.py#L121) · [调用规则](../appai/middleware/prepare_ctrl_tool_limit.py#L225)

## 业务图中的本地工具与 HTTP 调用

| 工具 | 所属 Agent | 用途 |
| --- | --- | --- |
| `query_car_status` | 车况 | 调用车辆信号接口查询电量、门窗、胎压、实时充电状态等 |
| `query_maintain_info` | 车况 | 查询保养和健康报告 |
| `query_car_config` | 车况 | 查询本车车型及销售配置 |
| `resolve_time_tool` | 预约车控、预约备车 | 两个 Agent 各自注册同名的本地工具，解析自然语言时间 |
| `delete_vehicle_prep_tool` | 删除备车 | 通过 HTTP 删除备车预约 |

导航 POI 搜索使用高德 HTTP API，下发车机导航指令使用车控 MCP 的 `navi_ctrl`。车控确认提交也使用 HTTP。[导航查询](../appai/car_ctrl_navi/navigation_agent_node.py#L133) · [车控确认提交](../appai/car_ctrl_confirm/confirm_ctrl_cmds_tool.py#L180) · [删除备车工具](../appai/car_prepare/prepare_tools.py#L851)

## 仓库中的其他 MCP 定义

以下定义未接入上述业务图，未计入业务工具数量。

| 来源 | 工具或用途 |
| --- | --- |
| [高德 MCP 客户端](../appai/mcp_tools/amap_mcp_client.py) | 保留的客户端配置，本次业务 MCP 工具发现未查询该服务 |
| [备车 Mock](../appai_mock/mcp_complex_ctrl.py) | `car_prepare_ctrl`、`vehicle_prep_ctrl` |
| [车控 Mock](../appai/mock/mock_mcp_server.py) 及 [旧版 Mock](../appai/mock/ac_ctrl_mcp_server_bakv1.py) | `extract_car_window_ctrl_params`、`extract_car_ac_ctrl_params`、`extract_car_appoint_ac_ctrl_params` |
| [股票 Mock](../appai/mock/stock_mock_mcp.py) | `query_stock`、`extract_car_ac_ctrl_params`、`add`、`multiply` |
