# MQTT + UDP 协议契约

版本：`M0-v0.1`。以下是新探针的行为要求与参考实现推导，不表示目标固件已通过测试。消息样例均为合成输入。

## 1. 身份与 MQTT 层

设备以 MQTT 3.1.1 CONNECT 连接，使用预登记 `client_id`、用户名和独立密码。身份从 Broker 成功鉴权的连接获取，不从 JSON 的 `device_id`、`session_id` 或 MAC 推断。

参考常见配置：

- `client_id` 形如 `GID_homevoice@@@02_00_00_00_00_01@@@02_00_00_00_00_01`。
- 上行主题为共享的 `device-server`。
- 下行主题形如 `devices/p2p/{设备标识}`；标识使用冒号还是下划线，以目标固件实际配置为准。

新探针只接受配置中登记的精确字符串，不用正则匹配替代凭据校验。每设备只能发布获准上行主题、订阅自己的精确下行主题，不允许 `#`、`+`、其他设备或管理员主题。控制消息不允许 retained，防止重连执行旧指令。

普通订阅客户端收到的 MQTT 3.1.1 PUBLISH 不自带已认证发布者身份。共享上行主题必须从 Broker 的可信回调获得发布者和连接代次；不能相信 payload 里自行添加的身份字段。

必须验证 CONNECT/CONNACK、QoS 0 与目标固件使用的 QoS 1/PUBACK、SUBSCRIBE/SUBACK、PINGREQ/PINGRESP、DISCONNECT。不承诺持久会话、QoS 2 或消息恰好一次执行；不支持时明确拒绝。

Broker 连接与音频会话分离。同一身份重连时旧连接退出不能清掉新连接登记。心跳以入站活动为准；出站成功不证明设备在线。

## 2. HELLO 与两个会话层级

设备上行：

```json
{
  "type": "hello",
  "version": 3,
  "transport": "udp",
  "features": {"mcp": true},
  "audio_params": {
    "format": "opus",
    "sample_rate": 16000,
    "channels": 1,
    "frame_duration": 60
  }
}
```

`version=3` 是合成候选，不可据此假定所有固件支持。未知或不支持的格式应拒绝并报告，不能静默降级到 WebSocket。能力声明只用于协商，不是权限。

服务器向该 MQTT 身份的下行主题回复 `type=hello`，包含：

| 字段 | 含义 |
|---|---|
| `transport` | `udp` |
| `session_id` | 协议音频会话标识，与 MQTT 连接代次分别管理 |
| `audio_params` | 服务器实际采用的下行音频格式 |
| `udp.server` / `port` | 设备可达地址与 UDP 端口，不能返回容器内部地址 |
| `udp.key` | 16 字节新随机密钥，32 位十六进制 |
| `udp.nonce` | 16 字节模板，32 位十六进制，含 type/flags/SSRC |

登记新音频会话后才能下发 HELLO。重发 HELLO 必须替换密钥、SSRC、协议标识、源地址和序列状态，并取消旧任务；旧 SSRC 包不能继续使用。

`goodbye` 释放音频会话但不等于断开 MQTT。设备可以保持 Idle、接收通知，再发 HELLO 建立新 UDP 会话。

## 3. UDP 帧与密码处理

线上为 16 字节明文头 + AES-128-CTR 加密载荷：

| 字节 | 字段 | 格式 |
|---|---|---|
| 0 | packet type | 模板值，通常 `0x01` |
| 1 | flags | 模板值，通常 `0x00` |
| 2–3 | payload length | 大端 uint16 |
| 4–7 | SSRC | 大端 uint32，对应当前音频会话 |
| 8–11 | timestamp | 大端 uint32，参考使用毫秒 |
| 12–15 | sequence | 大端 uint32 |

AES 密钥为 HELLO 下发值。**每个包自己的完整 16 字节头才是初始计数器**，不是固定 HELLO nonce。CTR 按 128 位大端计数器递增。公开向量见[测试数据](fixtures/udp-vectors.json)，它验证密码封装，不是有效 Opus。

1500 字节是本探针 UDP 应用数据报上限，不是无 IP 分片保证。路径 MTU 为 1500 时还要扣除 IP/UDP 头；隧道会进一步降低可用载荷。真机记录实际 Opus 帧大小和丢包，必要时采用更小的发送上限，不扩大接收内存限制。

先检查总长度、声明长度、type/flags、有效 SSRC，再处理来源与序列。`payload_len=0` 可用于纯封装测试，但音频入口不得送入解码器。

入站重放策略：维护最高序列和 64 包滑动窗口，窗口内未见序号可接受、重复或更旧包拒绝；音频按时间/序列有界处理，不承诺恢复丢失包。首个序号为 0 的兼容性须真实记录，窗口不能把尚未初始化误当已见 0。

源地址在首次满足结构、会话及配置来源限制的包时绑定，此后同会话来源变化拒绝，不用新包覆盖地址。NAT 变化要求重新 HELLO。此绑定仍不是密码学认证。

出站必须使用接收 UDP 的同一个 socket；先收到允许的上行包才能确定回包地址。下行序号从 1 递增，失败不能回退；回绕前重建会话。下行时间戳不为 0，不能将它宣称为完整的双向 IV 分离方案。

AES-CTR 篡改载荷不会产生认证失败。测试必须证明这一限制，而不是期望错误密钥/篡改必然被拒绝。对端抢占、双向计数器复用/重叠、包注入及网络窃听属于公网设计门槛。

## 4. 语音时序与状态

| 输入/动作 | 探针行为 |
|---|---|
| `listen start`, mode=`manual` | 开始收音，等待 stop 或上限 |
| mode=`auto` / `realtime` | 有界收音；模拟结束信号/超时必须能收口 |
| `listen detect`, text | 可记录为测试输入，不能当作身份或授权 |
| `listen stop` | 结束本轮收音，进入处理/播放 |
| 播放中 `abort` | 取消当前轮，保留 MQTT 和有效 UDP 会话；后续 start 可复用 |
| 收音中 `abort` | 参考把它作为结束收音；若目标固件不同须记录决策 |
| `goodbye` / MQTT 断开 | 释放音频和待执行请求；MQTT 断开还清理在线身份 |

播放顺序为 MQTT `tts start` → 可取消 settle → UDP 预缓冲及节奏发送 → 可取消尾等待 → MQTT `tts stop`。

两条链路没有天然顺序保证。150 ms settle、5 帧预缓冲与尾等待 `(5+2)×frame_duration` 只是参考起点，必须真机验证。慢云服务恢复后不能一次性追赶发送所有历史帧。

上下行采样率分别保存。参考常见上行 16 kHz、下行 24 kHz，不按单一全局配置解码/编码。PCM 分块保留残余采样，流式编码使用同一个 Opus 状态；结束时按已验证策略补齐尾帧。

## 5. Idle 通知

MQTT 在线、UDP 会话不存在且空闲时，向精确下行主题发送：

```json
{
  "type": "notify",
  "text": "Homevoice 测试通知",
  "audio_url": "https://test-audio.invalid/notice.wav"
}
```

URL 仅为合成样例，不可用于真实下载。目标固件需验证媒体格式、HTTPS 信任、URL 长度、下载授权与播放行为。

M0 对忙碌设备返回 `busy`、对离线设备返回 `offline`，不强制中断、不排无限队列、不自动补播。区分 `published` 与 `played`；未有固件确认只能记录发送与外部观察。旧固件可能完全不支持 notify，即使 MQTT 正常。

## 6. 设备 MCP 与远程 MCP

设备 MCP 通过 MQTT 应用消息包裹 JSON-RPC：

```json
{
  "type": "mcp",
  "payload": {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
      "protocolVersion": "2024-11-05",
      "capabilities": {},
      "clientInfo": {"name": "homevoice-m0", "version": "0.1.0"}
    }
  }
}
```

随后按响应与目标协议要求完成初始化通知，`tools/list` 支持 cursor 分页，工具名和完整 schema 从响应获取。示例协议版本需真机确认；不照搬参考固定 id 或省略初始化步骤。

每连接使用唯一递增请求 ID，登记 `(连接代次, 协议会话, request ID)`；迟到、重复、其他设备或旧会话响应不能完成当前请求。JSON-RPC `error` 或 `result.isError=true` 均不是成功。

真实工具名称、参数和范围不预设。模型仅看到 `set_current_device_volume(level)` 合成工具，由执行器映射已发现且获准的设备工具；不得直接开放全部设备工具。

远程 HA MCP 与设备 MCP 是不同目标。M0 只查询授权测试端点的工具或合成只读数据，不调用真实 HA 写服务；验证需要的 SSE/Streamable HTTP 选定一种，记录鉴权与超时。
