# 参考研究与证据记录

研究日期：2026-10-22。研究者为用户指定的规格负责人；本文件已提炼必要结论，执行 Agent 不得根据下面的来源记录自行跨仓库查阅。

## 基线与证据级别

| 参考 | 提交 |
|---|---|
| haimen | `325aad519b98e6ceb833e965cd641bc1620e11d1` |
| xiaozhi-esp32-server | `eb9dc8ca0fc8a30bc8afb0f302dcfc1578f76a7b` |

相关参考源码未发现已跟踪修改。haimen 有与本研究无关的未跟踪文件，未将其作为协议证据。

证据等级：

- **S：源码支持**，尚未在新 Go 实现或目标固件验证。
- **V：独立局部验证**，只证明明确运行的局部性质。
- **H：真实硬件验证**，本次无此等级结果。

旧源码中的测试和作者实测注释均归 S，不冒充本项目运行证据。没有复制旧实现代码；公开协议事实和合成样例由规格负责人整理。

## 已核实结论

| ID | 等级 | 来源标识 | 结论与新项目影响 |
|---|---|---|---|
| E01 | S | haimen `mqtt_broker.rs` 的 BrokerIncoming/handle_packet | 上行事件附带连接 client_id；共享主题身份应由可信 Broker 回调传递 |
| E02 | S | haimen `mqtt_broker.rs` 的 handle_connection/unregister_if_current | 重连用连接代次清理，避免旧连接摘除新连接；凭据可选开启，不沿用这种默认 |
| E03 | S | haimen `mqtt_udp.rs` 的 handle_hello | HELLO 下发随机 key、nonce、SSRC、协议标识；重复 HELLO 注销旧音频会话 |
| E04 | S | haimen `protocol_udp.rs` | 16 字节大端头是每包 CTR 初始计数器，不是固定 nonce；无认证标签 |
| E05 | V | 本轮 OpenSSL AES-128-CTR | 20 字节公开向量得到 `c0cd5ad06c57c0901c95f5bc025d6d3f8e59916b`；只验证密码封装 |
| E06 | S | haimen `udp_audio.rs` | 包上限 1500，同 socket 回包，首次源地址锁定；入站未实现完整重放窗口 |
| E07 | S | haimen `mqtt_udp.rs`、`udp_audio.rs` | 下行序号从 1、时间戳非零，试图避开首包 IV 碰撞；不构成完整双向认证或计数器分离证明 |
| E08 | S | haimen `mqtt_udp.rs` 的 wait_tts_start_settle/wait_tail_playback | MQTT start 与 UDP 首帧存在竞态；settle、预缓冲、尾等待应可取消 |
| E09 | S | haimen `mqtt_udp.rs` 的 handle_abort/goodbye | 播放 abort 保留 UDP，goodbye 释放音频但保持 MQTT；两层生命周期不同 |
| E10 | S | haimen `mqtt_udp.rs` 的在线门面、`notify.rs` | Idle 通知走在线 MQTT 身份，不依赖现有 UDP 会话；通知需可达音频 URL |
| E11 | S | haimen `mqtt_udp.rs` 的旧固件注释 | 作者注明固件 2.2.6 对 notify 无反应；不能推断本用户固件必然支持 |
| E12 | S | haimen `xiaozhi_asr_llm_tts.rs`、`xiaozhi_tts.rs` | 常见上行 16 kHz、下行 24 kHz/60 ms；连续 Opus 状态与尾帧不可丢失 |
| E13 | S | haimen `mqtt_udp.rs` 的消息分派 | 分派 hello/listen/abort/goodbye，未提供本需求的完整设备 MCP 路由；新探针需新增 |
| E14 | S | 小智服务 `core/providers/tools/device_mcp/mcp_handler.py` | 外层 mcp/内层 JSON-RPC、初始化、工具分页与调用；参考固定请求号不可直接复用 |
| E15 | S | 小智服务 `core/handle/textHandler/mcpMessageHandler.py` | 设备 MCP 可作为应用消息处理，但不能由此证明目标固件的 MQTT 支持 |
| E16 | S | 小智服务 `core/api/ota_handler.py`、DeviceServiceImpl 的 buildMqttConfig | MQTT 配置含 endpoint/client_id/username/password/publish_topic/subscribe_topic；共享上行、按设备下行 |
| E17 | S | 小智服务的 MQTT 分支与网关配置 | 本地服务依赖独立 MQTT 网关；该外部网关不在本轮已核实源码范围 |
| E18 | S | 小智服务 `core/providers/tools/server_mcp/mcp_client.py` | 远程 MCP 支持 SSE/Streamable HTTP 配置、鉴权和超时；与设备 MCP 分开 |

## 独立向量验证方法

仅公开合成输入，未接触真实设备或费用服务：

```bash
printf '%s' '00112233445566778899aabbccddeeff00112233' \
  | xxd -r -p \
  | openssl enc -aes-128-ctr \
      -K 00112233445566778899aabbccddeeff \
      -iv 01a00014112233440000003c00000001 \
  | xxd -p
```

输出：`c0cd5ad06c57c0901c95f5bc025d6d3f8e59916b`。样例见[向量文件](fixtures/udp-vectors.json)。此验证不证明 Go 实现、音频可播放或 UDP 抗篡改。

## 不继承的行为

- 鉴权可关闭、共享迁移密码、根据自报 MAC 任意发凭据。
- 不登记订阅转发而直接按连接下发的简化 Broker 行为；优先正确使用成熟库和 ACL。
- 无认证标签却把解密返回值作为可信来源。
- 任意客户发布管理控制主题、无限排队或不记录取消代次。
- 固定 MCP 请求 ID、仅按 id 匹配、不确认结果就宣称成功。

## 剩余研究问题

真实固件版本、TLS/证书和配置下发、通知媒体与结果、MQTT MCP 工具和 schema、上下行实际计数器序列、HA MCP 传输，以及 Go Broker/Opus 库兼容性均待验证。

对于旧源码未涵盖的关键问题，执行 Agent 提交证据请求，由规格负责人补充资料；不得自行访问第三方旧网关或其他私人项目。
