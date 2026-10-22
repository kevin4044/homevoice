# Offline compatibility findings (M0-v0.1 candidate)

Status: **offline implementation candidate only**. This file records what the
offline simulator can and cannot establish. It does not modify the
specification's compatibility matrix and is not a hardware result. Items below
are not evidence for M0 acceptance.

| Item | Offline status | Evidence / limitation | Remaining action |
|---|---|---|---|
| Go toolchain and dependency lock | PASS (offline) | `make check-deps`, `offline-report.md` dependencies table; go.sum locked | none for offline |
| MQTT broker trusted publisher identity | PASS (simulated) | A03/A04, broker tests: identity comes from authenticated CONNECT, payload identity rejected at app layer | verify with target firmware topics/credentials |
| Per-device MQTT ACL / non-retained control | PASS (simulated) | A04: wildcard/other-device/admin/retained rejected; downlink isolated | verify exact firmware topic strings and broker behaviour |
| MQTT 3.1.1 handshake, QoS0/1, keepalive, limits | PASS (simulated) | A06 with paho clients and raw malformed/oversized TCP | verify firmware MQTT client behaviour and broker quirk handling |
| MQTT TLS / certificate trust | Implemented but **unverified** | TLS server support and verified client TLS config; no insecure skip; live/hardware not executed | endpoint format, certificate chain, rejection evidence on hardware |
| MQTT CONNECT/HELLO waits, max packet | PASS (simulated) | A06/limit tests: 5 s CONNECT timeout, 64 KiB | confirm with firmware and real network |
| Exact client_id/publish/subscribe topics | Synthetic only | loopback synthetic identities in tests | obtain exact firmware strings + configuration write method |
| UDP 16-byte header + AES-128-CTR vectors | PASS (public vector) | A08/A13: byte-exact vector, fixed-nonce misuse rejected, tampering has no integrity error | confirm firmware implements header-as-counter |
| UDP length/template/SSRC/source/peer/replay | PASS (simulated) | A09-A12; first-source binding is explicitly not authentication | measure firmware sequence/timestamp/NAT behaviour |
| UDP first sequence / timestamp / SSRC rotation | Partially simulated | first sequence 0 explicitly tested; downlink starts at 1 and timestamp non-zero; SSRC rotate on HELLO | record actual firmware values and reuse/overlap |
| Opus encode/decode and streaming | PASS (libopus 1.6.1) | A14/A15 (synthetic sine/silence, uneven chunks, tail) | confirm firmware codec/frame sizes and audio quality |
| Uplink/downlink audio parameters | Candidate only | 16 kHz mono uplink, 24 kHz mono 60 ms downlink in config/tests | measure firmware formats and tolerances |
| Playback timing: settle/prebuffer/pacing/tail | PASS (fake clock) | A16: 150 ms/5 frames/60 ms/(5+2) frames; no burst catch-up | tune to firmware, record real latency/packets |
| Abort / goodbye / reconnect lifecycle | PASS (simulated) | A17/A18: abort keeps MQTT+UDP, goodbye keeps MQTT, disconnect clears both | verify on firmware, including wake-word abort |
| Idle notify without UDP session | PASS (simulated publish) | A20: publish while no UDP session; busy/offline explicit; published != played | verify firmware downloads/plays media and notification latency |
| Notify HTTPS/media format/URL limits | Synthetic server only | notify payloads use synthetic URLs; simulator provides a loopback HTTPS WAV server with certificate-pinned client (default trust rejects the self-signed cert) | authorize real HTTPS media, verify firmware trust/format/length/playback |
| Device MCP initialize/paging/correlation/errors | PASS (simulated) | A21-A23 with paho-like fake sender and fixture tool; full schema preserved | enumerate real firmware tools/schema |
| Session-bound volume tool loop | PASS (simulated) | A24/A25; only `set_current_device_volume` exposed; model cannot target another device | confirm real tool mapping and volume range |
| Remote MCP read-only query | PASS (simulated HTTP/TLS) | A26 with httptest; 401/TLS/timeout/write-tool refusal; no HA writes | authorize read-only endpoint and run L03 |
| Cloud ASR/LLM/TTS | Not executed | live entry blocked; no provider/budget supplied | authorize one provider, round budget and price cap |
| Real device HELLO/audio/notify/MCP | Not executed | no device/firmware/network/restore authorization | provide compatibility.md inputs, run H01-H06 |
| OTA/theme/credential provisioning | Not implemented (out of scope) | M0 only investigates; no firmware or credential service | decide in M1 after M0 acceptance |

Security conclusions to carry forward:

- AES-CTR has no authentication tag; peer locking, template/SSRC matching and
  the replay window are not authentication. Public deployment requires a tunnel,
  firmware change or AEAD-capable protocol.
- Bidirectional CTR counter/IV overlap and first-packet peer pre-emption are
  unresolved public-network risks.
- MQTT publish success is not proof of device playback.
- No real HA write tools were called in any evidence path.
