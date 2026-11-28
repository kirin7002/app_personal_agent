# MCP 工具 Schema

工具用途、注册状态和验证范围见 [业务 MCP 工具清单](./业务%20MCP%20工具清单.md)。

本节保留工具定义中的字段类型、必填项（`required`）、枚举、默认值和嵌套结构。展开工具名可查看完整 JSON。

- MCP 服务端 schema 来自 dev 环境的 `tools/list`。本次返回的 31 个业务工具都提供了 `inputSchema`，均未声明 `outputSchema`。仓库另有车控响应示例与车况响应示例。
- Agent 封装及本地工具 schema 使用当前代码中 LangChain 工具转换后的函数参数结构，并统一放在 `inputSchema` 下展示。车况封装的 `runtime: object` 出现在转换结果中，执行时由 `StatusNode` 用当前请求上下文注入并覆盖。车辆身份等上下文信息由应用处理。车况工具执行 · MCP 请求头注入
- Mock schema 来自仓库内各 FastMCP 实例的工具定义。Mock 声明了 `outputSchema` 时一并列出。

## 容易混淆的参数

| 工具 | MCP 服务端参数 | Agent 工具参数 |
| --- | --- | --- |
| `car_appointment_record` | `cmdidList` 必填，`deviceKey` 可选 | `cmdidList` 默认 `[161, 171, 361]`，可用 `target_date` 和 `sub_items` 筛选。封装调用 MCP 时只传 `cmdidList`，日期和子项在本地筛选 |
| `car_airconditioner_status` | 无业务参数 | `names` 供本地筛选字段，为空时取默认字段 |
| `car_charging_info`、`car_permission_info` | `deviceKey` 可选 | 无业务参数 |
| `resolve_time_tool` | 本地工具 | 预约车控和预约备车各有一套 schema。预约车控版本额外支持周期类型、重复天和结束时间等参数 |

## MCP 服务端定义

### 车控（remote）

<details>
<summary><code>ac_appoint_ctrl</code></summary>

```json
{
  "name": "ac_appoint_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "premode": {
        "type": "string",
        "description": "预设模式，['1'=打开极速制冷,'2'=打开极速制热,'3'=打开快速除味,'4'=打开风挡除霜,'5'=关闭风挡除霜,'6'=关闭极速制冷,'7'=关闭极速制热, '8'=关闭快速除味]",
        "enum": [
          "1",
          "2",
          "3",
          "4",
          "5",
          "6",
          "7",
          "8"
        ]
      },
      "operate": {
        "type": "string",
        "description": "操作模式，['auto'=自动,'manual'=手动]",
        "enum": [
          "auto",
          "manual"
        ]
      },
      "power": {
        "type": "string",
        "description": "空调开关，['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "temperature": {
        "type": "string",
        "description": "空调温度，数字整型，如 20"
      },
      "temperature_tune": {
        "type": "string",
        "description": "温度相对调节值，字符串格式如 '+3' 表示升高3度，'-2' 表示降低2度。与 temperature 互斥，同时传入时以此参数为准"
      },
      "wind_level": {
        "type": "string",
        "description": "空调风速档位，数字整型，如 2"
      },
      "wind_level_tune": {
        "type": "string",
        "description": "风速相对调节值，字符串格式如 '+1' 表示升高1档，'-2' 表示降低2档。与 wind_level 互斥，同时传入时以此参数为准"
      },
      "mode": {
        "type": "string",
        "description": "空调模式，['nohotcold'=通风,'hot'=制热,'cold'=制冷]",
        "enum": [
          "nohotcold",
          "hot",
          "cold"
        ]
      },
      "windshield_switch": {
        "type": "string",
        "description": "风挡加热开关，['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "circle": {
        "type": "string",
        "description": "内外循环模式，['in'=内循环,'out'=外循环]",
        "enum": [
          "in",
          "out"
        ]
      },
      "start_time": {
        "type": "string",
        "description": "预约执行时间，格式 yyyy-MM-dd HH:mm:ss"
      },
      "days": {
        "type": "string",
        "description": "每周重复日列表，['0'=周日,'1'=周一,'2'=周二,'3'=周三,'4'=周四,'5'=周五,'6'=周六]，如 ['2','3'] 为每周二和周三重复执行，每天为['0','1','2','3','4','5','6']，仅执行一次则设为 []"
      }
    },
    "required": [
      "power",
      "start_time",
      "days"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>ac_ctrl</code></summary>

```json
{
  "name": "ac_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "premode": {
        "type": "string",
        "description": "预设模式，['1'=打开极速制冷,'2'=打开极速制热,'3'=打开快速除味,'4'=打开风挡除霜,'5'=关闭风挡除霜,'6'=关闭极速制冷,'7'=关闭极速制热, '8'=关闭快速除味]",
        "enum": [
          "1",
          "2",
          "3",
          "4",
          "5",
          "6",
          "7",
          "8"
        ]
      },
      "operate": {
        "type": "string",
        "description": "操作模式，['auto'=自动,'manual'=手动]",
        "enum": [
          "auto",
          "manual"
        ]
      },
      "power": {
        "type": "string",
        "description": "空调开关，['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "temperature": {
        "type": "string",
        "description": "空调温度，数字整型，如 20"
      },
      "temperature_tune": {
        "type": "string",
        "description": "温度相对调节值，字符串格式如 '+3' 表示升高3度，'-2' 表示降低2度。与 temperature 互斥，同时传入时以此参数为准"
      },
      "wind_level": {
        "type": "string",
        "description": "空调风速档位，数字整型，如 2"
      },
      "wind_level_tune": {
        "type": "string",
        "description": "风速相对调节值，字符串格式如 '+1' 表示升高1档，'-2' 表示降低2档。与 wind_level 互斥，同时传入时以此参数为准"
      },
      "mode": {
        "type": "string",
        "description": "空调模式，['nohotcold'=通风,'hot'=制热,'cold'=制冷]",
        "enum": [
          "nohotcold",
          "hot",
          "cold"
        ]
      },
      "windshield_switch": {
        "type": "string",
        "description": "风挡加热开关，['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "circle": {
        "type": "string",
        "description": "内外循环模式，['in'=内循环,'out'=外循环]",
        "enum": [
          "in",
          "out"
        ]
      }
    },
    "required": [
      "power"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>bat_heat_ctrl</code></summary>

```json
{
  "name": "bat_heat_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['ptcon'=开, 'ptcoff'=关]",
        "enum": [
          "ptcon",
          "ptcoff"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>car_lock_ctrl</code></summary>

```json
{
  "name": "car_lock_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['unlock'=解锁,'lock'=闭锁]",
        "enum": [
          "unlock",
          "lock"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>car_window_ctrl</code></summary>

```json
{
  "name": "car_window_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "open_pct": {
        "type": "string",
        "description": "车窗位置,['0'=关闭,'2'=微开,'5'=打开||半开,'10'=全开]",
        "enum": [
          "0",
          "2",
          "5",
          "10"
        ]
      }
    },
    "required": [
      "open_pct"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>charge_start_stop_ctrl</code></summary>

```json
{
  "name": "charge_start_stop_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['start'=开始充电,'finish'=结束充电]",
        "enum": [
          "start",
          "finish"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>charger_unlock_ctrl</code></summary>

```json
{
  "name": "charger_unlock_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['unlock'=解锁]",
        "enum": [
          "unlock"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>child_seat_vent_ctrl</code></summary>

```json
{
  "name": "child_seat_vent_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "power": {
        "type": "string",
        "description": "开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "mac": {
        "type": "string",
        "description": "儿童座椅设备 mac 地址，不传时默认控制所有座椅"
      }
    },
    "required": [
      "power"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>deeplink_ctrl</code></summary>

```json
{
  "name": "deeplink_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "feature": {
        "type": "string",
        "description": "功能名称，可选值: app_control, fota, fridge, schedule_sync, park_photo, ble_key, message_center, sentinel_mode, forest_oxygen, bind_pile, custom_scene",
        "enum": [
          "app_control",
          "fota",
          "fridge",
          "schedule_sync",
          "park_photo",
          "ble_key",
          "message_center",
          "sentinel_mode",
          "forest_oxygen",
          "bind_pile",
          "custom_scene"
        ]
      },
      "deviceType": {
        "type": "string",
        "description": "设备类型，可选值: ios, android, harmony",
        "enum": [
          "ios",
          "android",
          "harmony"
        ]
      }
    },
    "required": [
      "feature",
      "deviceType"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>front_trunk_ctrl</code></summary>

```json
{
  "name": "front_trunk_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['0'=关,'100'=开]",
        "enum": [
          "0",
          "100"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>healthy_charge_ctrl</code></summary>

```json
{
  "name": "healthy_charge_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>horn_honk_ctrl</code></summary>

```json
{
  "name": "horn_honk_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['true'=打开|鸣笛，'false'=关闭]",
        "enum": [
          "true",
          "false"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>mirror_heat_ctrl</code></summary>

```json
{
  "name": "mirror_heat_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "power": {
        "type": "string",
        "description": "开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      }
    },
    "required": [
      "power"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>navi_ctrl</code></summary>

```json
{
  "name": "navi_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "name": {
        "type": "string",
        "description": "目的地地址名称"
      },
      "longitude": {
        "type": "string",
        "description": "目的地经度"
      },
      "latitude": {
        "type": "string",
        "description": "目的地纬度"
      },
      "address": {
        "type": "string",
        "description": "目的地详细地址"
      },
      "id": {
        "type": "string",
        "description": "高德地图 POI ID"
      }
    },
    "required": [
      "name",
      "longitude",
      "latitude",
      "address",
      "id"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>row23_mode_ctrl</code></summary>

```json
{
  "name": "row23_mode_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['reset'=三排座椅打开||二排复位,'back'=二排座椅打开||三排座椅关闭]",
        "enum": [
          "reset",
          "back"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>seat_heat_ctrl</code></summary>

```json
{
  "name": "seat_heat_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "level": {
        "type": "integer",
        "description": "加热挡位，数字整型 (1-3 档)",
        "minimum": 1,
        "maximum": 3
      },
      "power": {
        "type": "string",
        "description": "座椅加热开关，['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "positionList": {
        "type": "string",
        "description": "座椅位置，多个位置用英文逗号分隔。可选值：left_front(主驾/一排左侧)、right_front(副驾/一排右侧)、left_rear(二排左侧)、right_rear(二排右侧)、left_third(三排左侧)、right_third(三排右侧)。示例：\"left_front,right_front\" 表示第一排"
      }
    },
    "required": [
      "positionList"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>seat_vent_ctrl</code></summary>

```json
{
  "name": "seat_vent_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "level": {
        "type": "integer",
        "description": "通风挡位，数字整型 (1-3 档)",
        "minimum": 1,
        "maximum": 3
      },
      "power": {
        "type": "string",
        "description": "座椅通风开关，['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "positionList": {
        "type": "string",
        "description": "座椅位置，多个位置用英文逗号分隔。可选值：left_front(主驾/一排左侧)、right_front(副驾/一排右侧)、left_rear(二排左侧)、right_rear(二排右侧)。示例：\"left_front,right_front\" 表示第一排"
      }
    },
    "required": [
      "positionList"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>sliding_door_ctrl</code></summary>

```json
{
  "name": "sliding_door_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "positionList": {
        "type": "string",
        "description": "侧滑门位置，多个位置用英文逗号分隔。可选值：secondRowPassengerSide(右侧滑门)、secondRowDriverSide(左侧滑门)。示例：\"secondRowPassengerSide\" 表示右侧"
      },
      "operate": {
        "type": "string",
        "description": "操作，['0'=关，'100'=开]",
        "enum": [
          "0",
          "100"
        ]
      }
    },
    "required": [
      "positionList",
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>steer_wheel_heat_ctrl</code></summary>

```json
{
  "name": "steer_wheel_heat_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "power": {
        "type": "string",
        "description": "开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      }
    },
    "required": [
      "power"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>sunroof_ctrl</code></summary>

```json
{
  "name": "sunroof_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "天窗操作,['0'=关闭天窗,'1'=天窗通风,'10'=天窗全开]",
        "enum": [
          "0",
          "1",
          "10"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>sunshade_ctrl</code></summary>

```json
{
  "name": "sunshade_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "遮阳帘操作,['0'=全关,'1'=半开,'10'=全开]",
        "enum": [
          "0",
          "1",
          "10"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>timed_bat_heat_ctrl</code></summary>

```json
{
  "name": "timed_bat_heat_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "power": {
        "type": "string",
        "description": "预约电池预热开关,['on'=开,'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "start_time": {
        "type": "string",
        "description": "预约执行时间，格式 yyyy-MM-dd HH:mm:ss"
      },
      "days": {
        "type": "string",
        "description": "每周重复日列表，['0'=周日,'1'=周一,'2'=周二,'3'=周三,'4'=周四,'5'=周五,'6'=周六]，如 ['2','3'] 为每周二和周三重复执行，每天为['0','1','2','3','4','5','6']，仅执行一次则设为 []"
      }
    },
    "required": [
      "power"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>timed_charge_ctrl</code></summary>

```json
{
  "name": "timed_charge_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "power": {
        "type": "string",
        "description": "预约充电开关,['on'=开,'off'=关]，不传时仅设置其他参数如充电上限",
        "enum": [
          "on",
          "off"
        ]
      },
      "starttime": {
        "type": "string",
        "description": "开始时间，格式 HH:mm，如 22:00，不传时下发填充默认值"
      },
      "endtime": {
        "type": "string",
        "description": "结束时间，格式 HH:mm，如 06:00，不传时下发填充默认值"
      },
      "chargesoc": {
        "type": "string",
        "description": "充电上限百分比，数字整型，50-90"
      },
      "chargesoc_tune": {
        "type": "string",
        "description": "充电上限相对调节值，字符串格式如 '+5' 表示上调5个百分点，'-10' 表示下调10个百分点。与 chargesoc 互斥，同时传入时以此参数为准"
      },
      "recharge": {
        "type": "string",
        "description": "到停止时间未达到充电上限是否继续充电，['0'=不继续,'1'=继续]",
        "enum": [
          "0",
          "1"
        ]
      },
      "cyclesList": {
        "type": "array",
        "items": {
          "type": "integer",
          "format": "int32"
        },
        "description": "每周重复日列表，元素为星期编号['1'=周一,'2'=周二,'3'=周三,'4'=周四,'5'=周五,'6'=周六,'0'=周日]，只传选中的日期，如[2]表示每周二重复执行，[1,5]表示每周一和周五重复执行，[1,2,3,4,5,6,0]表示每天重复执行，[]表示仅执行一次"
      },
      "circulation": {
        "type": "string",
        "description": "启用循环预约，['0'=关闭,'1'=开启]",
        "enum": [
          "0",
          "1"
        ]
      }
    },
    "required": [],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>trunk_ctrl</code></summary>

```json
{
  "name": "trunk_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "operate": {
        "type": "string",
        "description": "操作,['true'=开,'false'=关]",
        "enum": [
          "true",
          "false"
        ]
      }
    },
    "required": [
      "operate"
    ],
    "additionalProperties": false
  }
}
```

</details>

### 车况（car_status）

<details>
<summary><code>car_airconditioner_status</code></summary>

```json
{
  "name": "car_airconditioner_status",
  "inputSchema": {
    "type": "object",
    "properties": {},
    "required": [],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>car_appointment_record</code></summary>

```json
{
  "name": "car_appointment_record",
  "inputSchema": {
    "type": "object",
    "properties": {
      "cmdidList": {
        "type": "array",
        "items": {
          "type": "integer",
          "format": "int32"
        },
        "description": "指令ID列表，例如: [1, 2, 3]"
      },
      "deviceKey": {
        "type": "string",
        "description": "设备Key过滤条件，为空时不过滤"
      }
    },
    "required": [
      "cmdidList"
    ],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>car_charging_info</code></summary>

```json
{
  "name": "car_charging_info",
  "inputSchema": {
    "type": "object",
    "properties": {
      "deviceKey": {
        "type": "string",
        "description": "设备Key"
      }
    },
    "required": [],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>car_permission_info</code></summary>

```json
{
  "name": "car_permission_info",
  "inputSchema": {
    "type": "object",
    "properties": {
      "deviceKey": {
        "type": "string",
        "description": "设备Key"
      }
    },
    "required": [],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>car_signal_status</code></summary>

```json
{
  "name": "car_signal_status",
  "inputSchema": {
    "type": "object",
    "properties": {
      "deviceKey": {
        "type": "string",
        "description": "设备Key"
      }
    },
    "required": [],
    "additionalProperties": false
  }
}
```

</details>

### 备车（complex-ctrl）

<details>
<summary><code>car_prepare_ctrl</code></summary>

```json
{
  "name": "car_prepare_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "ac_premode": {
        "type": "string",
        "description": "[空调] 预设模式，['1'=极速制冷，'2'=极速制热，'3'=快速除味，'4'=风挡除霜开，'5'=风挡除霜关]",
        "enum": [
          "1",
          "2",
          "3",
          "4",
          "5"
        ]
      },
      "ac_operate": {
        "type": "string",
        "description": "[空调] 操作模式，['auto'=自动，'manual'=手动]",
        "enum": [
          "auto",
          "manual"
        ]
      },
      "ac_power": {
        "type": "string",
        "description": "[空调] 开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "ac_temperature": {
        "type": "string",
        "description": "[空调] 温度，数字整型，如 20"
      },
      "ac_wind_level": {
        "type": "string",
        "description": "[空调] 风速档位，数字整型，如 2"
      },
      "ac_mode": {
        "type": "string",
        "description": "[空调] 模式，['nohotcold'=通风，'hot'=制热，'cold'=制冷，'hotcold'=制冷制热]",
        "enum": [
          "nohotcold",
          "hot",
          "cold",
          "hotcold"
        ]
      },
      "ac_windshield_switch": {
        "type": "string",
        "description": "[空调] 风挡加热开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "ac_circle": {
        "type": "string",
        "description": "[空调] 内外循环模式，['in'=内循环，'out'=外循环]",
        "enum": [
          "in",
          "out"
        ]
      },
      "navi_name": {
        "type": "string",
        "description": "[导航] 目的地地址名称"
      },
      "navi_longitude": {
        "type": "string",
        "description": "[导航] 目的地经度"
      },
      "navi_latitude": {
        "type": "string",
        "description": "[导航] 目的地纬度"
      },
      "navi_address": {
        "type": "string",
        "description": "[导航] 目的地详细地址"
      },
      "navi_id": {
        "type": "string",
        "description": "[导航] 高德地图 POI ID"
      },
      "seat_left_front": {
        "type": "string",
        "description": "[座椅设置] 主驾，['3'=加热，'13'=通风]",
        "enum": [
          "3",
          "13"
        ]
      },
      "seat_right_front": {
        "type": "string",
        "description": "[座椅设置] 副驾，[3'=加热，'13'=通风]",
        "enum": [
          "3",
          "13"
        ]
      },
      "seat_left_rear": {
        "type": "string",
        "description": "[座椅设置] 二排左侧，['3'=加热，'13'=通风]",
        "enum": [
          "3",
          "13"
        ]
      },
      "seat_right_rear": {
        "type": "string",
        "description": "[座椅设置] 二排右侧，['3'=加热，'13'=通风]",
        "enum": [
          "3",
          "13"
        ]
      },
      "seat_left_third": {
        "type": "string",
        "description": "[座椅设置] 三排左侧，['3'=加热]",
        "enum": [
          "3"
        ]
      },
      "seat_right_third": {
        "type": "string",
        "description": "[座椅设置] 三排右侧，['3'=加热]",
        "enum": [
          "3"
        ]
      },
      "steer_wheel_heat_power": {
        "type": "string",
        "description": "[方向盘加热] 开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "rear_mirror_heat_power": {
        "type": "string",
        "description": "[后视镜加热] 开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "child_seat_vent_power": {
        "type": "string",
        "description": "[儿童座椅通风] 开关，['on'=开，'off'=关]",
        "enum": [
          "on",
          "off"
        ]
      },
      "child_seat_vent_mac": {
        "type": "string",
        "description": "[儿童座椅通风] 儿童座椅 mac 地址，不传时默认控制所有座椅"
      }
    },
    "required": [],
    "additionalProperties": false
  }
}
```

</details>

<details>
<summary><code>vehicle_prep_ctrl</code></summary>

```json
{
  "name": "vehicle_prep_ctrl",
  "inputSchema": {
    "type": "object",
    "properties": {
      "start_time": {
        "type": "string",
        "description": "预约执行时间，支持格式：yyyy-MM-dd HH:mm:ss、yyyy-MM-dd HH:mm、yyyy-MM-dd HH、yyyy-MM-dd"
      },
      "days": {
        "type": "string",
        "description": "每周重复日列表，['0'=周日,'1'=周一,'2'=周二,'3'=周三,'4'=周四,'5'=周五,'6'=周六]，如 ['2','3'] 为每周二和周三重复执行，每天为['0','1','2','3','4','5','6']，仅执行一次则设为 []"
      },
      "ac": {
        "type": "object",
        "properties": {
          "circle": {
            "type": "string",
            "description": "内外循环模式，['in'=内循环，'out'=外循环]",
            "enum": [
              "in",
              "out"
            ]
          },
          "mode": {
            "type": "string",
            "description": "模式，['nohotcold'=通风，'hot'=制热，'cold'=制冷，'hotcold'=制冷制热]",
            "enum": [
              "nohotcold",
              "hot",
              "cold",
              "hotcold"
            ]
          },
          "operate": {
            "type": "string",
            "description": "操作模式，['auto'=自动，'manual'=手动]",
            "enum": [
              "auto",
              "manual"
            ]
          },
          "power": {
            "type": "string",
            "description": "开关，['on'=开，'off'=关]",
            "enum": [
              "on",
              "off"
            ]
          },
          "premode": {
            "type": "string",
            "description": "预设模式，['1'=极速制冷，'2'=极速制热，'3'=快速除味，'4'=风挡除霜开，'5'=风挡除霜关]",
            "enum": [
              "1",
              "2",
              "3",
              "4",
              "5"
            ]
          },
          "temperature": {
            "type": "string",
            "description": "温度，数字整型，如 20"
          },
          "wind_level": {
            "type": "string",
            "description": "风速档位，数字整型，如 2"
          },
          "windshield_switch": {
            "type": "string",
            "description": "风挡加热开关，['on'=开，'off'=关]",
            "enum": [
              "on",
              "off"
            ]
          }
        },
        "description": "空调设置对象，仅需调节空调时填写，包含 premode/operate/power/temperature/wind_level/mode/windshield_switch/circle"
      },
      "navi": {
        "type": "object",
        "properties": {
          "address": {
            "type": "string",
            "description": "目的地详细地址"
          },
          "id": {
            "type": "string",
            "description": "高德地图 POI ID"
          },
          "latitude": {
            "type": "string",
            "description": "目的地纬度"
          },
          "longitude": {
            "type": "string",
            "description": "目的地经度"
          },
          "name": {
            "type": "string",
            "description": "目的地地址名称"
          }
        },
        "description": "导航设置对象，仅需设置导航时填写，包含 name/longitude/latitude/address/id"
      },
      "seat_setting": {
        "type": "object",
        "properties": {
          "left_front": {
            "type": "string",
            "description": "主驾，['-1'=关闭，'3'=加热，'13'=通风]",
            "enum": [
              "-1",
              "3",
              "13"
            ]
          },
          "left_rear": {
            "type": "string",
            "description": "二排左侧，['-1'=关闭，'3'=加热，'13'=通风]",
            "enum": [
              "-1",
              "3",
              "13"
            ]
          },
          "left_third": {
            "type": "string",
            "description": "三排左侧，['-1'=关闭，'3'=加热]",
            "enum": [
              "-1",
              "3"
            ]
          },
          "right_front": {
            "type": "string",
            "description": "副驾，['-1'=关闭，'3'=加热，'13'=通风]",
            "enum": [
              "-1",
              "3",
              "13"
            ]
          },
          "right_rear": {
            "type": "string",
            "description": "二排右侧，['-1'=关闭，'3'=加热，'13'=通风]",
            "enum": [
              "-1",
              "3",
              "13"
            ]
          },
          "right_third": {
            "type": "string",
            "description": "三排右侧，['-1'=关闭，'3'=加热]",
            "enum": [
              "-1",
              "3"
            ]
          }
        },
        "description": "座椅设置对象，仅需调节座椅时填写，包含 left_front/right_front/left_rear/right_rear/left_third/right_third"
      },
      "steer_wheel_heat": {
        "type": "object",
        "properties": {
          "power": {
            "type": "string",
            "description": "开关，['on'=开，'off'=关]",
            "enum": [
              "on",
              "off"
            ]
          }
        },
        "description": "方向盘加热对象，仅需开启时填写，包含 power"
      },
      "rear_mirror_heat": {
        "type": "object",
        "properties": {
          "power": {
            "type": "string",
            "description": "开关，['on'=开，'off'=关]",
            "enum": [
              "on",
              "off"
            ]
          }
        },
        "description": "后视镜加热对象，仅需开启时填写，包含 power"
      },
      "child_seat_vent": {
        "type": "object",
        "properties": {
          "mac": {
            "type": "string",
            "description": "儿童座椅 mac 地址，不传时默认控制所有座椅"
          },
          "power": {
            "type": "string",
            "description": "开关，['on'=开，'off'=关]",
            "enum": [
              "on",
              "off"
            ]
          }
        },
        "description": "儿童座椅通风对象，仅需开启时填写，包含 power 和可选 mac"
      }
    },
    "required": [
      "start_time",
      "days"
    ],
    "additionalProperties": false
  }
}
```

</details>

## Agent 封装及本地工具定义

### 车况（car_status）

<details>
<summary><code>query_car_config</code></summary>

```json
{
  "name": "query_car_config",
  "inputSchema": {
    "properties": {
      "runtime": {}
    },
    "required": [
      "runtime"
    ],
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>query_car_status</code></summary>

```json
{
  "name": "query_car_status",
  "inputSchema": {
    "properties": {
      "runtime": {},
      "device_names": {
        "description": "设备名称列表。查看基础车况时传['电池与充电','档位','车锁','胎压','车门','车窗','后备箱','后视镜','座椅','方向盘']。注意：空调数据请使用 car_airconditioner_status 工具",
        "items": {
          "enum": [
            "电池与充电",
            "续航",
            "车门",
            "车窗",
            "胎压",
            "座椅",
            "空调",
            "方向盘",
            "天窗",
            "前备箱",
            "后备箱",
            "遮阳帘",
            "后视镜",
            "车锁",
            "寻车",
            "档位",
            "AI停车信息",
            "停车信息",
            "权限",
            "儿童座椅配置",
            "侧滑门"
          ],
          "type": "string"
        },
        "type": "array"
      },
      "names": {
        "default": [],
        "description": "要提取的字段名称列表。**只能从枚举中选择，禁止捏造不存在的字段**。必须为每个 device_names 指定对应的字段名，例如查询寻车时指定['车辆位置']，查询车门时指定['左前车门状态','右前车门状态']等",
        "items": {
          "enum": [
            "当前电量",
            "当前充电状态",
            "剩余充电时间",
            "充电电流",
            "充电电压",
            "充电功率",
            "健康充电开关",
            "CLTC电续航里程",
            "CLTC油续航里程",
            "CLTC增程总续航里程",
            "WLTC电续航里程",
            "WLTC油续航里程",
            "WLTC增程总续航里程",
            "左前车门状态",
            "右前车门状态",
            "左侧滑门状态",
            "右侧滑门状态",
            "左前车窗状态",
            "右前车窗状态",
            "左后车窗状态",
            "右后车窗状态",
            "左前胎压力值",
            "右前胎压力值",
            "左后胎压力值",
            "右后胎压力值",
            "左前胎压力状态",
            "右前胎压力状态",
            "左后胎压力状态",
            "右后胎压力状态",
            "主驾座椅加热",
            "副驾座椅加热",
            "二排左侧座椅加热",
            "二排右侧座椅加热",
            "三排左侧座椅加热",
            "三排右侧座椅加热",
            "主驾座椅通风",
            "副驾座椅通风",
            "二排左侧座椅通风",
            "二排右侧座椅通风",
            "车内温度",
            "风速",
            "空调开关",
            "工作模式",
            "前除霜",
            "后除霜",
            "空调温度",
            "空调模式",
            "空调快速除味",
            "空调极速降温",
            "空调极速升温",
            "风挡除霜状态",
            "方向盘加热",
            "左后视镜加热",
            "右后视镜加热",
            "驻车状态",
            "车锁状态",
            "后备箱状态",
            "前备箱",
            "儿童座椅通风1",
            "儿童座椅通风2",
            "遮阳帘状态",
            "天窗状态",
            "车辆位置经度",
            "车辆位置纬度",
            "车辆位置",
            "AI停车位楼层",
            "停车图片Key",
            "AI停车位查询时间",
            "停车时间",
            "停车环境描述",
            "停车照片URL",
            "驻车照片URL",
            "上传时间",
            "GPS隐私权限",
            "数据隐私权限",
            "AI停车状态"
          ],
          "type": "string"
        },
        "type": "array"
      },
      "is_general_query": {
        "default": false,
        "description": "是否模糊查询，如'查看车况'时为 True",
        "type": "boolean"
      }
    },
    "required": [
      "runtime",
      "device_names"
    ],
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>query_maintain_info</code></summary>

```json
{
  "name": "query_maintain_info",
  "inputSchema": {
    "properties": {
      "runtime": {}
    },
    "required": [
      "runtime"
    ],
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>car_appointment_record</code></summary>

```json
{
  "name": "car_appointment_record",
  "inputSchema": {
    "properties": {
      "runtime": {},
      "cmdidList": {
        "default": [
          161,
          171,
          361
        ],
        "description": "预约类型ID列表。根据用户意图选择：161=预约电池预热，171=预约空调，361=预约备车。含'预约'的电池加热问法（'预约电池加热'、'预约电池预热'、'查看预约电池加热'、'预约电池加热状态'、'预约电池加热打开了吗'）一律按预约电池预热处理传[161]，禁止拒答；仅'查询电池加热'等完全不含'预约'的实时查询不支持。例如：'查看预约空调'（未指明查哪个）传[171,361]，'定时空调'/'预约的定时空调'（指明定时空调）只传[171]，'备车温度'/'备车空调'（指明备车里的空调）只传[361]，'查看电池预热'/'预约电池加热'传[161]，'查看预约备车'传[361]，'查看所有预约'或'查看预约情况'传[161,171,361]，且必须同时并行调用 car_charging_info 查询预约充电，缺一不可。禁止传 190：预约充电必须用 car_charging_info 工具查询",
        "items": {
          "enum": [
            161,
            171,
            361
          ],
          "type": "integer"
        },
        "type": "array"
      },
      "target_date": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "查询目标日期，格式：YYYY-MM-DD。用户说'明天'传明天对应日期，'后天'传后天日期。用户说'8月10号'传以当前日期为基准的当年8月10日。不传返回所有预约。"
      },
      "sub_items": {
        "anyOf": [
          {
            "items": {
              "type": "string"
            },
            "type": "array"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "备车记录子项列表。用户查询备车中的具体子项时传入，可选值：导航、空调、座椅、方向盘加热、后视镜加热。例如：'备车导航去哪'传['导航']，'预约空调'（未指明查哪个）传['空调']（cmdidList传[171,361]），'备车温度'/'备车空调'传['空调']（cmdidList只传[361]），'备车座椅设置'传['座椅']，'备车的导航和空调'传['导航','空调']。用户只查'预约备车'未提具体子项时不传，返回全部备车记录"
      }
    },
    "required": [
      "runtime"
    ],
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>car_airconditioner_status</code></summary>

```json
{
  "name": "car_airconditioner_status",
  "inputSchema": {
    "properties": {
      "runtime": {},
      "names": {
        "default": [],
        "description": "要提取的字段名称列表，如['车内温度']。用户未指定具体字段时传空列表，返回默认展示字段（车内温度、空调开关、空调温度、空调模式）",
        "items": {
          "type": "string"
        },
        "type": "array"
      }
    },
    "required": [
      "runtime"
    ],
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>car_charging_info</code></summary>

```json
{
  "name": "car_charging_info",
  "inputSchema": {
    "properties": {
      "runtime": {}
    },
    "required": [
      "runtime"
    ],
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>car_permission_info</code></summary>

```json
{
  "name": "car_permission_info",
  "inputSchema": {
    "properties": {
      "runtime": {}
    },
    "required": [
      "runtime"
    ],
    "type": "object"
  }
}
```

</details>

### 预约车控（appoint_ctrl）

<details>
<summary><code>resolve_time_tool</code></summary>

```json
{
  "name": "resolve_time_tool",
  "inputSchema": {
    "properties": {
      "time_str": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "只放**用户明确说的钟点**，或用户明确说了公历日期时才放日期。\n- 明确钟点：'下午3点' → '15'，'早上7点半' → '07:30'，'晚上8点' → '20'\n- **完整时间格式（HH:mm）**：'00:08' → '00:08'（不要拆分成 time_str='8' + time_period='凌晨'）\n- **完整时间格式（HH:mm）**：'15:30' → '15:30'，'08:45' → '08:45'\n- 明确公历日期+钟点：'7月31日下午3点' → '2026-07-31 15'\n- 明确公历日期（无钟点）：'7月31日' → '2026-07-31'\n- **明确公历日期+钟点（含'年'）**：'2027年1月3号9点' → '2027-01-03 09'（日期+钟点一起放 time_str，不要拆，relative_days 留 0）\n- **明确公历日期（含'年'，无钟点）**：'2027年1月3号' → '2027-01-03'\n- 用户明确说了当前这个时候/这个时刻：当前时间（假如是15:49) → '15:49'；如：每天这个时候充电 → time_str应该传当前时间\n- **✗ 用户没说具体钟点 → time_str 留空**，只传 time_period（如果有）\n- **✗ 禁止把'明天''后天''下周三'等相对时间算成具体日期填入 time_str** → 用 relative_days（直接传'下周一'等汉字，工具会自动算）\n- **✗ 用户明确说了公历日期时，严禁拆分到 relative_days**：如'2027年1月3号9点'应整体填 time_str='2027-01-03 09'，禁止 time_str='9' + relative_days=87\n- **✗ 用户只说相对时间（如'半小时后''20分钟后'）→ 只传 relative_minutes/relative_hours，time_str 留空**\n- **✗ 禁止拆分完整时间**：用户说'00:08'、'15:30'等完整时间格式时，直接传 time_str='00:08'，不要拆分成钟点+时段"
      },
      "time_period": {
        "default": "",
        "description": "用户是否说了**时段词**（凌晨/早上/上午/下午/晚上/傍晚）。\n- 用户只说钟点没带时段词（如'2点'）→ 留空 ''\n- 用户说了时段词 → 填对应值：'凌晨' / '早上' / '上午' / '中午' / '下午' / '傍晚' / '晚上' / '夜里'\n**模糊时段词规则**：用户只说时段没具体钟点（如'明天上午''后天下午''一会儿'），time_str 必须留空，只传 time_period 和 relative_days。严禁自行编造钟点。",
        "type": "string"
      },
      "festival_name": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "节日/节气名称（如'中秋'、'春节'、'处暑'）。\n用户提到节日时填此字段，代码会查准确日期。"
      },
      "festival_year": {
        "anyOf": [
          {
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "节日年份（整数）。"
      },
      "relative_years": {
        "default": 0,
        "description": "年偏移（如'明年'=1，'去年'=-1）",
        "type": "integer"
      },
      "relative_months": {
        "default": 0,
        "description": "月偏移（如'下个月'=1）",
        "type": "integer"
      },
      "relative_days": {
        "anyOf": [
          {
            "type": "integer"
          },
          {
            "type": "string"
          }
        ],
        "default": 0,
        "description": "**开始时间**的天数偏移，支持两种类型：\n- **int**：数字天数偏移（如'三天后'=3，'昨天'=-1，'明天'=1）\n- **str**：中文自然语言日期表达式，工具会自动解析为天数，避免大模型计算错误\n**支持的汉字格式**（直接填进 relative_days，工具自动算）：\n- '今天'\n- '后天'\n- '大后天'\n- '下周三'\n- '这周一'\n- '周一'\n**公历日期请填 time_str**，不要填到 relative_days（relative_days 只接受 int 或上面列出的汉字表达式，填公历字符串工具会报错）：\n- ✗ relative_days='2026-10-15'（错误：公历日期必须放 time_str）\n- ✗ relative_days='9月30号'（错误：公历日期必须放 time_str）\n**推荐用法**：\n- ✓ '下周一10点' → time_str='10', relative_days='下周一'\n- ✓ '每天这个时候充电' → relative_days='现在', periodic_days=[0,1,2,3,4,5,6], has_periodic_days=true（工具自动取当前时刻作为 time_str）\n- ✓ '每周现在预约电池加热' → relative_days='现在', periodic_days=[...根据周几], has_periodic_days=true\n- ✓ '三天后10点' → time_str='10', relative_days=3"
      },
      "relative_hours": {
        "default": 0,
        "description": "时偏移。\n- '前/后 X 小时' → relative_hours=±X\n- 无偏移 → 不传（默认 0）",
        "type": "integer"
      },
      "relative_minutes": {
        "default": 0,
        "description": "分偏移。\n- '前/后 X 分钟' → relative_minutes=±X\n- 无偏移 → 不传（默认 0）",
        "type": "integer"
      },
      "relative_seconds": {
        "default": 0,
        "description": "秒偏移。\n- 无偏移 → 不传（默认 0）",
        "type": "integer"
      },
      "has_periodic_days": {
        "default": false,
        "description": "**仅当用户明确提到循环重复时才设为 true**（如'每天'、'每周一'、'每工作日'、'每周末'等）。\n用户只说了具体时间（如'9:55开始'）、具体日期（如'明天'、'下周三'）、或仅一次的操作 → **设为 false**。\n**常见错误**：用户说'9:55开始，两个小时后结束' → has_periodic_days=false（不是每天！）\n**判断标准**：用户原话中是否包含'每'、'每天'、'每周'、'每日'等表示循环的词语？没有 → false。",
        "type": "boolean"
      },
      "periodic_days": {
        "anyOf": [
          {
            "items": {
              "type": "integer"
            },
            "type": "array"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "**仅当用户明确提到循环重复时才填写**（如'每天'、'每周一'等），否则不传或传 None。\n**判断标准**：用户原话中是否包含'每'、'每天'、'每周'、'每日'等表示循环的词语？没有 → 不传。\n**每周X：需要填；周X：只是某一个周X，不用填 periodic_days**\n数字对应：0=每周日，1=每周一，2=每周二，3=每周三，4=每周四，5=每周五，6=每周六\n每个值必须在 0-6 范围内，否则工具会报错。\n示例：\n- '每天' → [0,1,2,3,4,5,6]\n- '每周x点x分' → [0,1,2,3,4,5,6]\n- '每周一' → [1]\n- '每周一二三' → [1,2,3]\n- '每周三四五' → [3,4,5]\n- '每周一三五' → [1,3,5]\n- '每工作日' → [1,2,3,4,5]\n- '每周末' → [0,6]\n- '每周三五八' → [3,5,8]\n- xx点xx分，没提到循环日期的 → 不传（默认 None）\n- 这周X/今天/明天，表示仅一次的，用户没提到循环 → 不传（默认 []）\n- 用户只说了具体时间（如'9:55'），没提到'每天'等循环词 → 不传（默认 []）**常见错误场景**：\n- ✗ '9:55开始，两个小时后结束' → **不传 periodic_days**（用户没说'每天'！）\n- ✗ '明天下午3点充电' → **不传 periodic_days**（'明天'是仅一次）\n- ✗ '下周三10点充电' → **不传 periodic_days**（'下周三'是仅一次）\n- ✓ '每天9点充电' → periodic_days=[0,1,2,3,4,5,6]\n- ✓ '每周一8点充电' → periodic_days=[1]\n"
      },
      "cycle_type": {
        "default": "",
        "description": "用户提到的重复周期类型。\n- '每天'、'每周x'、'工作日'、'周末' → 'weekly'\n- 无重复 → 不传或留空 ''\n- **'每年'等 → 填'yearly'**\n- **'每个月'、'每月1号' → 填'monthly'，**\n",
        "type": "string"
      },
      "end_time_str": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "只填写结束的钟点（用户说充xx分钟，填end_relative_minutes，充xx小时填end_relative_hours）。\n- '结束时间：11点' → end_time_str='11'\n- '每天八点30分结束充电' → end_time_str='8:30'\n- '结束时间：明天10点' → end_time_str='10', end_relative_days=1\n- **✗ 禁止自行推断结束钟点**，用户没说就留空 ''\n- '': 无结束时间信息（留空字符串）\n如果只填了钟点没填日期，会自动继承开始时间的日期。"
      },
      "end_time_period": {
        "default": "",
        "description": "结束时间的时段词，规则同 time_period。\n- '充到晚上10点' → end_time_period='晚上'\n- '充到10点'（无时段词）→ 留空 ''\n填：凌晨/早上/上午/中午/下午/傍晚/晚上/夜里。没说就留空 ''。",
        "type": "string"
      },
      "end_relative_days": {
        "default": 0,
        "description": "结束时间相对于开始时间的天数偏移，用户说'充到明天'或'充到后天'时使用。\n- '充到明天' → end_relative_days=1\n- '第二天晚上12:30' → end_relative_days=1, end_time_str='12:30', end_time_period='晚上'\n- '充到后天10点' → end_time_str='10', end_relative_days=2\n- '今天晚上8点充到明天早上6点' → time_str='20', time_period='晚上', end_time_str='6', end_time_period='早上', end_relative_days=1",
        "type": "integer"
      },
      "end_relative_hours": {
        "default": 0,
        "description": "用户说了充电持续的小时数。\n- '充1.5小时' → end_relative_hours=1, end_relative_minutes=30\n- '开始时间：每天八点，操作内容：预约充电，充3小时' → end_relative_hours=3\n**互斥规则**：end_time_str 与 end_relative_hours/end_relative_minutes 互斥，同一句话只能出现一种结束方式。",
        "type": "integer"
      },
      "end_relative_minutes": {
        "default": 0,
        "description": "用户说了充电持续的分钟数。\n- '开始时间：每天八点，操作内容：预约充电，充30分钟' → end_relative_minutes=30\n- '10分钟后结束充电' → end_relative_minutes=10\n**互斥规则**：end_time_str 与 end_relative_hours/end_relative_minutes 互斥，同一句话只能出现一种结束方式。\n结束时间参数与开始时间参数**独立**，用户只说结束时间（如'10分钟后结束充电'）时，开始时间参数留空。",
        "type": "integer"
      }
    },
    "type": "object"
  }
}
```

</details>

### 预约备车（prepare_reserve）

<details>
<summary><code>resolve_time_tool</code></summary>

```json
{
  "name": "resolve_time_tool",
  "inputSchema": {
    "properties": {
      "time_str": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null
      },
      "time_period": {
        "default": "",
        "type": "string"
      },
      "festival_name": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null
      },
      "festival_year": {
        "anyOf": [
          {
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null
      },
      "relative_years": {
        "default": 0,
        "type": "integer"
      },
      "relative_months": {
        "default": 0,
        "type": "integer"
      },
      "relative_days": {
        "anyOf": [
          {
            "type": "integer"
          },
          {
            "type": "string"
          }
        ],
        "default": 0
      },
      "relative_hours": {
        "default": 0,
        "type": "integer"
      },
      "relative_minutes": {
        "default": 0,
        "type": "integer"
      },
      "relative_seconds": {
        "default": 0,
        "type": "integer"
      },
      "has_periodic_days": {
        "default": false,
        "type": "boolean"
      }
    },
    "type": "object"
  }
}
```

</details>

### 删除备车（prepare_delete）

<details>
<summary><code>delete_vehicle_prep_tool</code></summary>

```json
{
  "name": "delete_vehicle_prep_tool",
  "inputSchema": {
    "properties": {
      "indices": {
        "anyOf": [
          {
            "items": {
              "type": "integer"
            },
            "type": "array"
          },
          {
            "type": "null"
          }
        ],
        "default": null
      },
      "delete_all": {
        "default": false,
        "type": "boolean"
      }
    },
    "type": "object"
  }
}
```

</details>

## Mock 定义

### 备车 Mock

<details>
<summary><code>car_prepare_ctrl</code></summary>

```json
{
  "name": "car_prepare_ctrl",
  "inputSchema": {
    "properties": {
      "vin": {
        "description": "车辆 vin 码",
        "title": "Vin",
        "type": "string"
      },
      "ac_premode": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 预设模式，['1'=极速制冷，'2'=极速制热，'3'=快速除味，'4'=风挡除霜开，'5'=风挡除霜关]",
        "title": "Ac Premode"
      },
      "ac_operate": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 操作模式，['auto'=自动，'manual'=手动]",
        "title": "Ac Operate"
      },
      "ac_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 开关，['on'=开，'off'=关]",
        "title": "Ac Power"
      },
      "ac_temperature": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 温度，数字整型，如 20",
        "title": "Ac Temperature"
      },
      "ac_wind_level": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 风速档位，数字整型，如 2",
        "title": "Ac Wind Level"
      },
      "ac_mode": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 模式，['nohotcold'=通风，'hot'=制热，'cold'=制冷，'hotcold'=制冷制热]",
        "title": "Ac Mode"
      },
      "ac_windshield_switch": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 风挡加热开关，['on'=开，'off'=关]",
        "title": "Ac Windshield Switch"
      },
      "ac_circle": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 内外循环模式，['in'=内循环，'out'=外循环]",
        "title": "Ac Circle"
      },
      "navi_addressname": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地地址名称",
        "title": "Navi Addressname"
      },
      "navi_longitude": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地经度",
        "title": "Navi Longitude"
      },
      "navi_latitude": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地纬度",
        "title": "Navi Latitude"
      },
      "navi_address": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地详细地址",
        "title": "Navi Address"
      },
      "navi_addresskey": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 高德地图 POI ID",
        "title": "Navi Addresskey"
      },
      "seat_left_front": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 主驾，['0'=无操作，'3'=加热，'13'=通风]",
        "title": "Seat Left Front"
      },
      "seat_right_front": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 副驾，['0'=无操作，'3'=加热，'13'=通风]",
        "title": "Seat Right Front"
      },
      "seat_left_rear": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 二排左侧，['0'=无操作，'3'=加热，'13'=通风]",
        "title": "Seat Left Rear"
      },
      "seat_right_rear": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 二排右侧，['0'=无操作，'3'=加热，'13'=通风]",
        "title": "Seat Right Rear"
      },
      "seat_left_third": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 三排左侧，['0'=无操作，'3'=加热，'13'=通风]",
        "title": "Seat Left Third"
      },
      "seat_right_third": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 三排右侧，['0'=无操作，'3'=加热，'13'=通风]",
        "title": "Seat Right Third"
      },
      "steer_wheel_heat_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[方向盘加热] 开关，['on'=开，'off'=关]",
        "title": "Steer Wheel Heat Power"
      },
      "rear_mirror_heat_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[后视镜加热] 开关，['on'=开，'off'=关]",
        "title": "Rear Mirror Heat Power"
      },
      "child_seat_vent_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[儿童座椅通风] 开关，['on'=开，'off'=关]",
        "title": "Child Seat Vent Power"
      },
      "child_seat_vent_mac": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[儿童座椅通风] 儿童座椅 mac 地址，不传时默认控制所有座椅",
        "title": "Child Seat Vent Mac"
      }
    },
    "required": [
      "vin"
    ],
    "title": "car_prepare_ctrlArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "additionalProperties": true,
        "title": "Result",
        "type": "object"
      }
    },
    "required": [
      "result"
    ],
    "title": "car_prepare_ctrlOutput",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>vehicle_prep_ctrl</code></summary>

```json
{
  "name": "vehicle_prep_ctrl",
  "inputSchema": {
    "properties": {
      "vin": {
        "description": "车辆 vin 码",
        "title": "Vin",
        "type": "string"
      },
      "start_time": {
        "description": "预约执行时间，格式 yyyy-MM-dd HH:mm:ss",
        "title": "Start Time",
        "type": "string"
      },
      "days": {
        "default": "[]",
        "description": "每周重复日列表，['0'=周日,'1'=周一,...]，仅执行一次则设为[]",
        "title": "Days",
        "type": "string"
      },
      "ac_premode": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 预设模式",
        "title": "Ac Premode"
      },
      "ac_operate": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 操作模式",
        "title": "Ac Operate"
      },
      "ac_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 开关",
        "title": "Ac Power"
      },
      "ac_temperature": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 温度",
        "title": "Ac Temperature"
      },
      "ac_wind_level": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 风速档位",
        "title": "Ac Wind Level"
      },
      "ac_mode": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 模式",
        "title": "Ac Mode"
      },
      "ac_windshield_switch": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 风挡加热开关",
        "title": "Ac Windshield Switch"
      },
      "ac_circle": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[空调] 内外循环模式",
        "title": "Ac Circle"
      },
      "navi_addressname": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地地址名称",
        "title": "Navi Addressname"
      },
      "navi_longitude": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地经度",
        "title": "Navi Longitude"
      },
      "navi_latitude": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地纬度",
        "title": "Navi Latitude"
      },
      "navi_address": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 目的地详细地址",
        "title": "Navi Address"
      },
      "navi_addresskey": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[导航] 高德地图 POI ID",
        "title": "Navi Addresskey"
      },
      "seat_left_front": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 主驾",
        "title": "Seat Left Front"
      },
      "seat_right_front": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 副驾",
        "title": "Seat Right Front"
      },
      "seat_left_rear": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 二排左侧",
        "title": "Seat Left Rear"
      },
      "seat_right_rear": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 二排右侧",
        "title": "Seat Right Rear"
      },
      "seat_left_third": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 三排左侧",
        "title": "Seat Left Third"
      },
      "seat_right_third": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[座椅设置] 三排右侧",
        "title": "Seat Right Third"
      },
      "steer_wheel_heat_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[方向盘加热] 开关",
        "title": "Steer Wheel Heat Power"
      },
      "rear_mirror_heat_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[后视镜加热] 开关",
        "title": "Rear Mirror Heat Power"
      },
      "child_seat_vent_power": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[儿童座椅通风] 开关",
        "title": "Child Seat Vent Power"
      },
      "child_seat_vent_mac": {
        "anyOf": [
          {
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "[儿童座椅通风] MAC地址",
        "title": "Child Seat Vent Mac"
      }
    },
    "required": [
      "vin",
      "start_time"
    ],
    "title": "vehicle_prep_ctrlArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "additionalProperties": true,
        "title": "Result",
        "type": "object"
      }
    },
    "required": [
      "result"
    ],
    "title": "vehicle_prep_ctrlOutput",
    "type": "object"
  }
}
```

</details>

### 车控 Mock

<details>
<summary><code>extract_car_window_ctrl_params</code></summary>

```json
{
  "name": "extract_car_window_ctrl_params",
  "inputSchema": {
    "properties": {
      "power": {
        "anyOf": [
          {
            "enum": [
              "on",
              "off"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "车窗开关：'on'(打开), 'out'(关闭)",
        "title": "Power"
      },
      "open_pct": {
        "default": null,
        "description": "设定车窗打开百分百",
        "maximum": 100,
        "minimum": 0,
        "title": "Open Pct",
        "type": "integer"
      }
    },
    "title": "extract_car_window_ctrl_paramsArguments",
    "type": "object"
  },
  "outputSchema": {
    "additionalProperties": true,
    "title": "extract_car_window_ctrl_paramsDictOutput",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>extract_car_ac_ctrl_params</code></summary>

```json
{
  "name": "extract_car_ac_ctrl_params",
  "inputSchema": {
    "properties": {
      "temperature": {
        "anyOf": [
          {
            "description": "设定温度，范围必须在 18 到 32 度之间",
            "maximum": 32,
            "minimum": 18,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Temperature"
      },
      "wind_level": {
        "anyOf": [
          {
            "description": "设定风速等级，范围必须在 0 到 3 档之间",
            "maximum": 3,
            "minimum": 0,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Wind Level"
      },
      "mode": {
        "anyOf": [
          {
            "enum": [
              "hot",
              "cold",
              "hotcold"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "非必选，运行模式：'hot'(制热), 'cold'(制冷), 'hotcold'(自动/除湿)",
        "title": "Mode"
      },
      "circle": {
        "anyOf": [
          {
            "enum": [
              "in",
              "out"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "循环模式：'in'(内循环), 'out'(外循环)",
        "title": "Circle"
      },
      "windshield_switch": {
        "anyOf": [
          {
            "enum": [
              "on",
              "off"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "除霜开关：'on'(打开), 'off'(关闭)",
        "title": "Windshield Switch"
      }
    },
    "title": "extract_car_ac_ctrl_paramsArguments",
    "type": "object"
  },
  "outputSchema": {
    "additionalProperties": true,
    "title": "extract_car_ac_ctrl_paramsDictOutput",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>extract_car_appoint_ac_ctrl_params</code></summary>

```json
{
  "name": "extract_car_appoint_ac_ctrl_params",
  "inputSchema": {
    "properties": {
      "days": {
        "anyOf": [
          {
            "enum": [
              "1",
              "2",
              "3",
              "4",
              "5",
              "6",
              "7"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "description": "必选，运行模式：'1'(周一), '2'(周二), '3'(周三)",
        "title": "Days"
      },
      "start_time": {
        "description": "任务执行时间，必须为 ISO 8601 格式 (YYYY-MM-DDTHH:MM:SS)，例如 2026-04-13T15:30:00",
        "pattern": "^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}$",
        "title": "Start Time",
        "type": "string"
      },
      "temperature": {
        "anyOf": [
          {
            "description": "设定温度，范围必须在 18 到 32 度之间",
            "maximum": 32,
            "minimum": 18,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Temperature"
      },
      "wind_level": {
        "anyOf": [
          {
            "description": "设定风速等级，范围必须在 0 到 3 档之间",
            "maximum": 3,
            "minimum": 0,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Wind Level"
      },
      "mode": {
        "anyOf": [
          {
            "enum": [
              "hot",
              "cold",
              "hotcold"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "非必选，运行模式：'hot'(制热), 'cold'(制冷), 'hotcold'(自动/除湿)",
        "title": "Mode"
      },
      "circle": {
        "anyOf": [
          {
            "enum": [
              "in",
              "out"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "循环模式：'in'(内循环), 'out'(外循环)",
        "title": "Circle"
      },
      "windshield_switch": {
        "anyOf": [
          {
            "enum": [
              "on",
              "off"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "除霜开关：'on'(打开), 'off'(关闭)",
        "title": "Windshield Switch"
      }
    },
    "required": [
      "days",
      "start_time"
    ],
    "title": "extract_car_appoint_ac_ctrl_paramsArguments",
    "type": "object"
  },
  "outputSchema": {
    "additionalProperties": true,
    "title": "extract_car_appoint_ac_ctrl_paramsDictOutput",
    "type": "object"
  }
}
```

</details>

### 旧版车控 Mock

<details>
<summary><code>extract_car_window_ctrl_params</code></summary>

```json
{
  "name": "extract_car_window_ctrl_params",
  "inputSchema": {
    "properties": {
      "power": {
        "anyOf": [
          {
            "enum": [
              "on",
              "off"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "车窗开关：'on'(打开), 'out'(关闭)",
        "title": "Power"
      },
      "open_pct": {
        "default": null,
        "description": "设定车窗打开百分百",
        "maximum": 100,
        "minimum": 0,
        "title": "Open Pct",
        "type": "integer"
      }
    },
    "title": "extract_car_window_ctrl_paramsArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "items": {
          "additionalProperties": true,
          "type": "object"
        },
        "title": "Result",
        "type": "array"
      }
    },
    "required": [
      "result"
    ],
    "title": "extract_car_window_ctrl_paramsOutput",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>extract_car_ac_ctrl_params</code></summary>

```json
{
  "name": "extract_car_ac_ctrl_params",
  "inputSchema": {
    "properties": {
      "temperature": {
        "anyOf": [
          {
            "description": "设定温度，范围必须在 18 到 32 度之间",
            "maximum": 32,
            "minimum": 18,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Temperature"
      },
      "wind_level": {
        "anyOf": [
          {
            "description": "设定风速等级，范围必须在 0 到 3 档之间",
            "maximum": 3,
            "minimum": 0,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Wind Level"
      },
      "mode": {
        "anyOf": [
          {
            "enum": [
              "hot",
              "cold",
              "hotcold"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "非必选，运行模式：'hot'(制热), 'cold'(制冷), 'hotcold'(自动/除湿)",
        "title": "Mode"
      },
      "circle": {
        "anyOf": [
          {
            "enum": [
              "in",
              "out"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "循环模式：'in'(内循环), 'out'(外循环)",
        "title": "Circle"
      },
      "windshield_switch": {
        "anyOf": [
          {
            "enum": [
              "on",
              "off"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "除霜开关：'on'(打开), 'off'(关闭)",
        "title": "Windshield Switch"
      }
    },
    "title": "extract_car_ac_ctrl_paramsArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "items": {
          "additionalProperties": true,
          "type": "object"
        },
        "title": "Result",
        "type": "array"
      }
    },
    "required": [
      "result"
    ],
    "title": "extract_car_ac_ctrl_paramsOutput",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>extract_car_appoint_ac_ctrl_params</code></summary>

```json
{
  "name": "extract_car_appoint_ac_ctrl_params",
  "inputSchema": {
    "properties": {
      "days": {
        "anyOf": [
          {
            "enum": [
              "1",
              "2",
              "3",
              "4",
              "5",
              "6",
              "7"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "description": "必选，运行模式：'1'(周一), '2'(周二), '3'(周三)",
        "title": "Days"
      },
      "start_time": {
        "description": "任务执行时间，必须为 ISO 8601 格式 (YYYY-MM-DDTHH:MM:SS)，例如 2026-04-13T15:30:00",
        "pattern": "^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}$",
        "title": "Start Time",
        "type": "string"
      },
      "temperature": {
        "anyOf": [
          {
            "description": "设定温度，范围必须在 18 到 32 度之间",
            "maximum": 32,
            "minimum": 18,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Temperature"
      },
      "wind_level": {
        "anyOf": [
          {
            "description": "设定风速等级，范围必须在 0 到 3 档之间",
            "maximum": 3,
            "minimum": 0,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Wind Level"
      },
      "mode": {
        "anyOf": [
          {
            "enum": [
              "hot",
              "cold",
              "hotcold"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "非必选，运行模式：'hot'(制热), 'cold'(制冷), 'hotcold'(自动/除湿)",
        "title": "Mode"
      },
      "circle": {
        "anyOf": [
          {
            "enum": [
              "in",
              "out"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "循环模式：'in'(内循环), 'out'(外循环)",
        "title": "Circle"
      },
      "windshield_switch": {
        "anyOf": [
          {
            "enum": [
              "on",
              "off"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "除霜开关：'on'(打开), 'off'(关闭)",
        "title": "Windshield Switch"
      }
    },
    "required": [
      "days",
      "start_time"
    ],
    "title": "extract_car_appoint_ac_ctrl_paramsArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "items": {
          "additionalProperties": true,
          "type": "object"
        },
        "title": "Result",
        "type": "array"
      }
    },
    "required": [
      "result"
    ],
    "title": "extract_car_appoint_ac_ctrl_paramsOutput",
    "type": "object"
  }
}
```

</details>

### 股票 Mock

<details>
<summary><code>query_stock</code></summary>

```json
{
  "name": "query_stock",
  "inputSchema": {
    "properties": {
      "stock_name": {
        "anyOf": [
          {
            "enum": [
              "lp",
              "hw"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "股票名称：'lp'(零跑), 'hw'(华为)",
        "title": "Stock Name"
      }
    },
    "title": "query_stockArguments",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>extract_car_ac_ctrl_params</code></summary>

```json
{
  "name": "extract_car_ac_ctrl_params",
  "inputSchema": {
    "properties": {
      "temperature": {
        "anyOf": [
          {
            "description": "设定温度，范围必须在 18 到 32 度之间",
            "maximum": 32,
            "minimum": 18,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Temperature"
      },
      "wind_level": {
        "anyOf": [
          {
            "description": "设定风速等级，范围必须在 0 到 3 档之间",
            "maximum": 3,
            "minimum": 0,
            "type": "integer"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "title": "Wind Level"
      },
      "mode": {
        "anyOf": [
          {
            "enum": [
              "hot",
              "cold",
              "hotcold"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "非必选，运行模式：'hot'(制热), 'cold'(制冷), 'hotcold'(自动/除湿)",
        "title": "Mode"
      },
      "circle": {
        "anyOf": [
          {
            "enum": [
              "in",
              "out"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "循环模式：'in'(内循环), 'out'(外循环)",
        "title": "Circle"
      },
      "windshield_switch": {
        "anyOf": [
          {
            "enum": [
              "on",
              "off"
            ],
            "type": "string"
          },
          {
            "type": "null"
          }
        ],
        "default": null,
        "description": "除霜开关：'on'(打开), 'off'(关闭)",
        "title": "Windshield Switch"
      }
    },
    "title": "extract_car_ac_ctrl_paramsArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "items": {
          "additionalProperties": true,
          "type": "object"
        },
        "title": "Result",
        "type": "array"
      }
    },
    "required": [
      "result"
    ],
    "title": "extract_car_ac_ctrl_paramsOutput",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>add</code></summary>

```json
{
  "name": "add",
  "inputSchema": {
    "properties": {
      "a": {
        "title": "A",
        "type": "integer"
      },
      "b": {
        "title": "B",
        "type": "integer"
      }
    },
    "required": [
      "a",
      "b"
    ],
    "title": "addArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "title": "Result",
        "type": "integer"
      }
    },
    "required": [
      "result"
    ],
    "title": "addOutput",
    "type": "object"
  }
}
```

</details>

<details>
<summary><code>multiply</code></summary>

```json
{
  "name": "multiply",
  "inputSchema": {
    "properties": {
      "a": {
        "title": "A",
        "type": "integer"
      },
      "b": {
        "title": "B",
        "type": "integer"
      }
    },
    "required": [
      "a",
      "b"
    ],
    "title": "multiplyArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {
      "result": {
        "title": "Result",
        "type": "integer"
      }
    },
    "required": [
      "result"
    ],
    "title": "multiplyOutput",
    "type": "object"
  }
}
```

</details>
