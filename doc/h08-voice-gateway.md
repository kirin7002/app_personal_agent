# H08 车端音频网关

将 `gateway.Gateway` 挂到 `GET /voice/audio`，注入会话认证、S2S、H02 轮次服务及候选读取、H05 结果读取、可信文本 TTS。网关按 [H00](vehicle-contract.md) 协商 PCM16LE（输入 16 kHz，输出 24 kHz）和控制帧。明确日常短句直接使用 S2S，车辆相关、短补参和无法确定的候选交给 H02。

车端起讲时立即停止本地播放，网关停止旧播放并取消在途工作。若取消发生在 Qwen 提交或生成的关联 ID 尚未确认时，网关更换连接代数并提示重新采集。已登记的业务命令由 Go 继续跟踪。重连时，网关从结果读取接口获取保留期内的对应业务事件。播放 ID 跨重连稳定，车端应持久化已经消费的 ID。网关进程内保存回执和插话后的播放状态，进程重启后使用稳定 ID 与车端已消费记录去重。

通过注入的 slog Logger 记录供应商就绪、首个输入分片、录音结束、最终转写、分流、Hermes 返回和首帧下行的耗时。需对真实音频的冷、热连接及闲聊、车控分别采样后再统计 P50/P95。

## 验证

```sh
go test -race ./voice/gateway -count=3 -v
go test -race ./...
go vet ./...
git diff --check
./scripts/car-audio-samples.sh /tmp/app-personal-agent-car-samples
```

模拟 WebSocket 验证通过。测试覆盖闲聊原生出声、插话停止旧播放及取消 Qwen、重复分片和轮次、旧代数隔离、结果断线后恢复，以及结果事件去重。生成的音频是 macOS 普通话语音合成样本。

连接部署时，使用模拟车端发送两轮原始 PCM。配置与网关鉴权匹配的 `CAR_SIM_TOKEN` 后执行。

```sh
go run ./cmd/car-sim -url ws://127.0.0.1:8080/voice/audio -wait-result -out /tmp/car-replies /tmp/app-personal-agent-car-samples/close-window.pcm /tmp/app-personal-agent-car-samples/rear-left.pcm
```

车端模拟器按间隔发送分片、保存下行音频并发送播放回执。H07 验证交接到实际 Hermes 和车辆任务模块的链路。
