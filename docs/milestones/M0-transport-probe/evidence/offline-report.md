# M0 offline verification evidence

- Specification: `M0-v0.1` (docs/milestones/M0-transport-probe)
- Generated: 2026-10-22T12:59:30Z
- Overall: **OFFLINE VERIFICATION PASSED (implementation candidate only; M0 acceptance still requires authorized live and hardware verification)**

> This report is offline-only. Cloud (L01-L03) and real-device (H01-H06) verification were **not executed** and are reported BLOCKED below. It is not an M0 acceptance decision; independent verification and the specification referee decide.

## Candidate

| Field | Value |
|---|---|
| Branch | `feature/m0-transport-probe` |
| HEAD commit | `460626c0105c05389fe99eb57f68b3365ae2a81f` |
| Working tree | dirty (uncommitted candidate; no commit/push authorized) |
| Go | `go1.27.1` (darwin/arm64, CGO=1) |
| libopus | `libopus 1.6.1` |

Approval note: the specification package still records status "待批准" (pending approval) in its README/spec/acceptance/handoff. The offline implementation was authorized by the user's direct instruction in this session; the specification owner/referee must confirm approval. No commit/push/PR was made, so this report describes the uncommitted working tree and its snapshot.

## Dependencies (locked in go.sum)

| Module | Version |
|---|---|
| `github.com/eclipse/paho.mqtt.golang` | `v1.5.1` |
| `github.com/gorilla/websocket` | `v1.5.3` |
| `github.com/mochi-mqtt/server/v2` | `v2.7.9` |
| `github.com/rs/xid` | `v1.4.0` |
| `golang.org/x/net` | `v0.44.0` |
| `golang.org/x/sync` | `v0.17.0` |

Package licenses (recorded for T0): mochi-mqtt MIT; paho.mqtt.golang EPL-2.0/EDL-1.0; gorilla/websocket BSD-3-Clause; golang.org/x/net BSD-3-Clause; golang.org/x/sync BSD-3-Clause; rs/xid MIT; libopus BSD-3-Clause. No dependency is copied from the retired project.

## Fixture integrity

| Fixture | SHA-256 |
|---|---|
| `control-messages.json` | `04f53ab66cd02a73e8965c866a9abca0ee5bf29475d1049cca66604082d30c96` |
| `udp-vectors.json` | `53ac9ccee8a55d20d6ee43bbeb272d7401c7718eb6af30b1cad9682c3538334e` |

## Offline gates

`make verify-m0` runs, in order: `gofmt -l` check, `go vet ./...`, `go test -race -count=1 ./...`, this report validation, and the working-tree snapshot. Gates:

| Gate | Status | Detail |
|---|---|---|
| gofmt | PASS | make fmt-check prerequisite |
| go-vet | PASS | make vet prerequisite |
| race | PASS | go test -race -count=1 ./... |

## Simulation scope (offline only)

- Simulated firmware device: MQTT 3.1.1 synthetic identity, UDP HELLO/session, Opus uplink/downlink with the public vectors, device MCP server (initialize/tools/list/tools/call), downlink notify/TTS observation (internal/m0/simulator).
- Simulated upstream: deterministic ASR/LLM/TTS with bounded latency, failure and slow modes; the model provider used by the agent loop is scripted (internal/m0/simulator).
- Integrated offline path: listen -> simulated ASR -> simulated TTS -> MQTT tts markers -> paced UDP frames, verified by `TestIntegratedListenTTSPlayback` inside `make verify-m0`.
- Synthetic HTTPS test audio server: loopback TLS with a runtime self-signed test certificate, WAV tone at /notice.wav; verification clients pin its certificate and never skip TLS verification (internal/m0/simulator).
- No real device, cloud, Home Assistant, OTA, public endpoint or retired-project component is contacted.

## Offline acceptance matrix (required)

| ID | Requirement | Status | Evidence |
|---|---|---|---|
| A01 | R01 | **PASS** | TestA01CleanRepositoryToolchain (github.com/homevoice/homevoice/internal/m0/acceptance) |
| A02 | R02 | **PASS** | TestA02IdentityAuthentication, TestA02IdentityAuthentication/credentials_of_another_device, TestA02IdentityAuthentication/empty_credentials, TestA02IdentityAuthentication/unknown_client_id, TestA02IdentityAuthentication/unknown_username, TestA02IdentityAuthentication/wrong_password (github.com/homevoice/homevoice/internal/m0/broker) |
| A03 | R03 | **PASS** | TestA03TrustedPublisherIdentity (github.com/homevoice/homevoice/internal/m0/broker) |
| A04 | R03 | **PASS** | TestA04ACLAndDownlinkIsolation, TestA04ACLAndDownlinkIsolation/subscribe_rejected_#, TestA04ACLAndDownlinkIsolation/subscribe_rejected_$SYS/#, TestA04ACLAndDownlinkIsolation/subscribe_rejected_+, TestA04ACLAndDownlinkIsolation/subscribe_rejected_admin/control, TestA04ACLAndDownlinkIsolation/subscribe_rejected_device-server, TestA04ACLAndDownlinkIsolation/subscribe_rejected_devices/#, TestA04ACLAndDownlinkIsolation/subscribe_rejected_devices/p2p/+, TestA04ACLAndDownlinkIsolation/subscribe_rejected_devices/p2p/02:00:00:00:00:02 (github.com/homevoice/homevoice/internal/m0/broker) |
| A05 | R02-R04 | **PASS** | TestA05DuplicateClientReconnect, TestA05ProbeTakeoverAcceptsNewGeneration (github.com/homevoice/homevoice/internal/m0/acceptance, github.com/homevoice/homevoice/internal/m0/broker) |
| A06 | R02 | **PASS** | TestA06HandshakeQoSAndBounds, TestA06HandshakeQoSAndBounds/connect_handshake_timeout_bounded, TestA06HandshakeQoSAndBounds/keepalive_closes_half-open_connection_using_inbound_activity_only, TestA06HandshakeQoSAndBounds/malformed_packets_bounded_and_broker_healthy, TestA06HandshakeQoSAndBounds/mqtt311_handshake_qos_puback_ping_disconnect, TestA06HandshakeQoSAndBounds/oversized_packet_rejected_bounded_and_broker_healthy, TestA06HandshakeQoSAndBounds/unsupported_protocol_versions_and_qos2_rejected_explicitly (github.com/homevoice/homevoice/internal/m0/broker) |
| A07 | R04 | **PASS** | TestA07HelloKeyRotationAndIsolation (github.com/homevoice/homevoice/internal/m0/session) |
| A08 | R05 | **PASS** | TestA08PublicVectorExactMatch (github.com/homevoice/homevoice/internal/m0/udp) |
| A09 | R05 | **PASS** | TestA09CodecRejectsMalformedFraming, TestA09SessionRejectsMalformedAndUnknownFrames, TestA09SessionRejectsMalformedAndUnknownFrames/oversize, TestA09SessionRejectsMalformedAndUnknownFrames/short_header, TestA09SessionRejectsMalformedAndUnknownFrames/truncated_payload, TestA09SessionRejectsMalformedAndUnknownFrames/unknown_ssrc, TestA09SessionRejectsMalformedAndUnknownFrames/wrong_template_flags, TestA09SessionRejectsMalformedAndUnknownFrames/wrong_template_type (github.com/homevoice/homevoice/internal/m0/session, github.com/homevoice/homevoice/internal/m0/udp) |
| A10 | R05 | **PASS** | TestA10ReplayWindowSemantics (github.com/homevoice/homevoice/internal/m0/session) |
| A11 | R05 | **PASS** | TestA11PeerBindingIsStickyButNotAuthentication (github.com/homevoice/homevoice/internal/m0/session) |
| A12 | R05 | **PASS** | TestA12OutboundSequenceAndFailureSemantics, TestA12OutboundUsesSameSocket (github.com/homevoice/homevoice/internal/m0/session) |
| A13 | R05,R12 | **PASS** | TestA13TamperedCiphertextHasNoAuthentication (github.com/homevoice/homevoice/internal/m0/udp) |
| A14 | R06 | **PASS** | TestA14DecoderRobustness, TestA14DirectionsIndependent, TestA14FixtureCiphertextIsNotAudio, TestA14SilenceRoundTrip, TestA14SyntheticSineRoundTrip, TestA14SyntheticSineRoundTrip/downlink_24k_mono_60ms, TestA14SyntheticSineRoundTrip/uplink_16k_mono_60ms (github.com/homevoice/homevoice/internal/m0/audio) |
| A15 | R06 | **PASS** | TestA15FlushTailStrategy, TestA15OddByteCountError, TestA15PCM16LittleEndian, TestA15StreamingStateIsolated, TestA15UnevenChunksNoSampleLoss, TestA15UnevenChunksNoSampleLoss/downlink_24k_mono_60ms, TestA15UnevenChunksNoSampleLoss/uplink_16k_mono_60ms (github.com/homevoice/homevoice/internal/m0/audio) |
| A16 | R07 | **PASS** | TestA16NoBurstAfterSlowUpstream, TestA16QueueBoundedAndDrops, TestA16SettlePrebufferCadenceTail, TestA16TailWaitUsesConfiguredFormula (github.com/homevoice/homevoice/internal/m0/playout) |
| A17 | R07 | **PASS** | TestA17AbortDuringSettle, TestA17AbortIsNoOpOutsideRound, TestA17AbortStopsSendsAndRoundReusable, TestA17AbortWinsOverContextCancellation, TestA17ContextCancellation, TestA17MaxQueueBoundMatchesSpec, TestA17RepeatedRoundsNoLeak (github.com/homevoice/homevoice/internal/m0/playout) |
| A18 | R07,R08 | **PASS** | TestA18AbortKeepsSessionGoodbyeKeepsMQTTDisconnectClearsBoth, TestA18GoodbyeMismatchPreservesSession (github.com/homevoice/homevoice/internal/m0/acceptance) |
| A19 | R07 | **PASS** | TestA19ProbeQueueAndTaskClosure, TestA19ResourceAccountingAfterManySessions (github.com/homevoice/homevoice/internal/m0/acceptance, github.com/homevoice/homevoice/internal/m0/session) |
| A20 | R08 | **PASS** | TestA20BusyAndOfflineFailExplicitly, TestA20IdleNotifyAcrossLifecycle, TestA20IdleNotifyDoesNotRequireUDP, TestA20OldPipelineMustNotClearNewBusy, TestA20OldPipelineMustNotClearNewBusy/rehello, TestA20OldPipelineMustNotClearNewBusy/same_session (github.com/homevoice/homevoice/internal/m0/acceptance, github.com/homevoice/homevoice/internal/m0/notify) |
| A21 | R09 | **PASS** | TestA21CursorCycleRejected, TestA21InitializeSendsClientInfoAndNotificationOnce, TestA21NonObjectToolsResultRejected, TestA21OversizedResponseRejected, TestA21PageLimitRejected, TestA21ToolLimitRejected, TestA21ToolsListPagination (github.com/homevoice/homevoice/internal/m0/devicemcp) |
| A22 | R09 | **PASS** | TestA22CancelFailsPendingAndLateResponseIgnored, TestA22IdentityCorrelation, TestA22ProbeSessionRotationIgnoresLateMCPResponse, TestA22SameRequestIDOnTwoDevicesDoesNotCross, TestA22TimeoutLateResponseAndDuplicateIgnored (github.com/homevoice/homevoice/internal/m0/acceptance, github.com/homevoice/homevoice/internal/m0/devicemcp) |
| A23 | R09 | **PASS** | TestA23CallToolNonObjectResultRejected, TestA23JSONRPCErrorIsFailure, TestA23SenderFailureDoesNotComplete, TestA23ToolErrorResultIsFailure, TestA23UnsupportedToolsNotExposed (github.com/homevoice/homevoice/internal/m0/devicemcp) |
| A24 | R10 | **PASS** | TestA24ExtraRequiredFieldIsNotExposed, TestA24MalformedOrOutOfRangeEnumIsRejected, TestA24OnlySyntheticSessionBoundTool, TestA24OptionalFieldIsAllowed, TestA24RejectsInvalidArgumentsAndForeignTools, TestA24RejectsInvalidArgumentsAndForeignTools/above_discovered_maximum, TestA24RejectsInvalidArgumentsAndForeignTools/array_arguments, TestA24RejectsInvalidArgumentsAndForeignTools/below_discovered_minimum, TestA24RejectsInvalidArgumentsAndForeignTools/bool_level, TestA24RejectsInvalidArgumentsAndForeignTools/device_parameter_instead_of_level, TestA24RejectsInvalidArgumentsAndForeignTools/device_tool_name, TestA24RejectsInvalidArgumentsAndForeignTools/extra_device_id, TestA24RejectsInvalidArgumentsAndForeignTools/extra_target_field, TestA24RejectsInvalidArgumentsAndForeignTools/float_level, TestA24RejectsInvalidArgumentsAndForeignTools/missing_level, TestA24RejectsInvalidArgumentsAndForeignTools/string_level, TestA24RestrictedEnumIsEnforced, TestA24SchemaMismatchNotExposed, TestA24SchemaMismatchNotExposed/empty_device_tool_name, TestA24SchemaMismatchNotExposed/exclusive_bounds, TestA24SchemaMismatchNotExposed/minimum_greater_than_maximum, TestA24SchemaMismatchNotExposed/missing_bounds, TestA24SchemaMismatchNotExposed/missing_root_type, TestA24SchemaMismatchNotExposed/no_unambiguous_volume_parameter, TestA24SchemaMismatchNotExposed/non-object_schema, TestA24SchemaMismatchNotExposed/string_volume_type, TestA24SchemaMismatchNotExposed/volume_not_required, TestA24SingleIntegerPropertyFallback, TestA24VolumeExecutorMapsSessionBoundDeviceTool (github.com/homevoice/homevoice/internal/m0/agent) |
| A25 | R10 | **PASS** | TestA25BlockedSenderExitsOnCancel, TestA25BlockedSenderExitsOnDeadline, TestA25CallerCancellationNotSuccess, TestA25DefaultsAreClamped, TestA25LegacyBlockedSenderBounded, TestA25ModelTextAloneIsNotSuccess, TestA25PreCanceledToolIsNotSent, TestA25ProviderErrorIsBoundedFailure, TestA25ProviderTimeoutNotSuccess, TestA25RequestLimit, TestA25ToolCallLimit, TestA25ToolCallLimit/batch_rejected_before_execution, TestA25ToolCallLimit/second_turn_tool_call_rejected, TestA25ToolFailureNotSuccess, TestA25ToolFailureNotSuccess/call_id_mismatch, TestA25ToolFailureNotSuccess/executor_transport_error, TestA25ToolFailureNotSuccess/isError_result (github.com/homevoice/homevoice/internal/m0/agent, github.com/homevoice/homevoice/internal/m0/devicemcp) |
| A26 | R11 | **PASS** | TestA26ConfigurationValidation, TestA26ConfigurationValidation/URL_with_credentials, TestA26ConfigurationValidation/URL_without_host, TestA26ConfigurationValidation/empty_URL, TestA26ConfigurationValidation/empty_allowlist_entry, TestA26ConfigurationValidation/excessive_timeout, TestA26ConfigurationValidation/negative_timeout, TestA26ConfigurationValidation/plaintext_URL, TestA26JSONRPCAndToolErrorsAreFailures, TestA26JSONRPCAndToolErrorsAreFailures/JSON-RPC_error, TestA26JSONRPCAndToolErrorsAreFailures/isError_result, TestA26OversizedResponseRejected, TestA26RefusesWriteToolWithoutHTTPRequest, TestA26RequiresInitializeAndHonorsClose, TestA26SSECancellationIsBounded, TestA26SSECompletesBeforeEOF, TestA26SSEErrorCompletesBeforeEOF, TestA26SSEEventStreamParsing, TestA26SSEIncompleteEventIsBounded, TestA26SSENoMatchIsBounded, TestA26SSEOversizedEventIsBounded, TestA26StreamableHTTPInitializeListAndReadOnlyCall, TestA26TLSVerificationFailure, TestA26Timeout, TestA26Unauthorized, TestA26Unauthorized/forbidden_target, TestA26Unauthorized/initialize_with_bad_token, TestA26Unauthorized/list_tools_unauthorized_after_initialize (github.com/homevoice/homevoice/internal/m0/remotemcp) |
| A27 | R12 | **PASS** | TestA27BindPolicyTLSAndNoDowngrade (github.com/homevoice/homevoice/internal/m0/acceptance) |
| A28 | R12 | **PASS** | TestA28LogsRedactSecretsAndAudio, TestA28MissingLiveAndHardwareConfigsBlockNonZero, TestA28MissingLiveAndHardwareConfigsBlockNonZero/hardware, TestA28MissingLiveAndHardwareConfigsBlockNonZero/live (github.com/homevoice/homevoice/cmd/m0probe, github.com/homevoice/homevoice/internal/m0/acceptance) |

## Authorized cloud / remote MCP (not executed)

| ID | Status | Reason |
|---|---|---|
| L01 | **BLOCKED** | live verification blocked: no --config path supplied; authorization file is required |
| L02 | **BLOCKED** | live verification blocked: no --config path supplied; authorization file is required |
| L03 | **BLOCKED** | live verification blocked: no --config path supplied; authorization file is required |

## Real hardware (not executed)

| ID | Status | Reason |
|---|---|---|
| H01 | **BLOCKED** | hardware verification blocked: no --config path supplied; authorization file is required |
| H02 | **BLOCKED** | hardware verification blocked: no --config path supplied; authorization file is required |
| H03 | **BLOCKED** | hardware verification blocked: no --config path supplied; authorization file is required |
| H04 | **BLOCKED** | hardware verification blocked: no --config path supplied; authorization file is required |
| H05 | **BLOCKED** | hardware verification blocked: no --config path supplied; authorization file is required |
| H06 | **BLOCKED** | hardware verification blocked: no --config path supplied; authorization file is required |

## Security limitations (must be carried into M1)

- UDP AES-128-CTR has no authentication tag (no MAC/AEAD). Tampered ciphertext decrypts to tampered plaintext without an integrity error; A13 proves this limitation instead of claiming integrity.
- UDP source-address locking, SSRC/template matching, the 64-packet replay window and Opus decoding are NOT authentication and must not be described as such.
- The first structurally valid packet locks the UDP peer of a fresh session; an on-path attacker can grab that binding before the device. Reconnection requires HELLO.
- SSRC is visible in cleartext; first-packet binding can be pre-empted. Bidirectional AES-CTR counter/IV reuse and overlap remain a public-network design risk; a non-zero downlink timestamp is not a proof of separation.
- The offline probe uses loopback plaintext for isolated tests. For authorized live/hardware configs the code requires MQTT TLS or an explicit isolated-plaintext approval (network.plaintext_isolated_approved) on a restricted bind; client TLS never disables verification and there is no automatic plaintext/WebSocket fallback. Authorized live/hardware execution was not performed in this run.
- Device WebSocket transport is out of M0 scope and is explicitly rejected rather than substituted.
- MQTT publish success is not playback confirmation; idle notify is reported as published, never played.
- Device MCP capability, notify media support, real Opus parameters, TLS/cert behaviour, NAT and firmware tools are unverified until authorized hardware runs.
- Remote MCP is limited to an authorized read-only test endpoint; real Home Assistant write tools are never called.

## Probe limits

- registered devices <= 2; extra identities rejected
- MQTT CONNECT handshake timeout 5s; HELLO wait 5s; MQTT idle does not require HELLO
- MQTT/JSON packet <= 64 KiB; UDP datagram <= 1500 bytes (payload <= 1484)
- per-device audio queue <= 64 frames, overflow counted, no unbounded allocation
- listen round <= 15s; upstream operation <= 30s; MCP request <= 5s
- tools/list <= 8 pages and <= 128 tools
- model loop: <= 2 requests and <= 1 tool execution per turn
- TTS candidate: settle 150ms, prebuffer 5 frames, tail wait (5+2)*frame_duration; downlink candidate opus mono 24kHz/60ms
- live cloud budget: <= 3 rounds, ASR <= 1 / LLM <= 2 / TTS <= 1 per round

## Known issues / open decisions

- The specification package still records status "pending approval" (待批准) in README/spec/acceptance/handoff. This coding run was authorized by the user's direct offline-implementation instruction; approval must be confirmed by the specification owner/referee and is not self-declared here.
- No commit/push/PR was authorized, so the candidate is the uncommitted working tree; see snapshot/ for HEAD, branch, dirty status and file hashes.
- Live cloud, remote MCP endpoint and real hardware were not executed; L01-L03 and H01-H06 remain BLOCKED. Firmware notify, device MCP, TLS, NAT and Opus timing therefore remain assumptions for M1 planning.
- The authorized live/hardware entry points currently validate authorization/config and then report BLOCKED; their actual L01-L03 / H01-H06 workflows are not implemented or validated in this candidate. They must not be recorded as pass until those workflows are implemented and exercised with authorization.
- The conditional T4 synthetic HTTPS configuration-distribution endpoint was not implemented because the target firmware configuration channel is unverified. A synthetic loopback HTTPS test-audio server (self-signed, certificate-pinned client) exists in the simulator for offline media tests, but real firmware notification download, media format and HTTPS trust remain untested on hardware.
- AES-CTR's lack of authentication must be resolved (tunnel, firmware change or AES-GCM-capable protocol) before any public deployment; this probe does not change the wire format.
- Thresholds such as 150ms settle, 5-frame prebuffer, 2 extra tail frames and 24kHz/60ms downlink are tunable candidates, not verified firmware parameters.
- The offline report generator enforces A01-A28 presence/pass; package-level tests named TestAxx are the evidence source.
- Additional sanitized evidence files: evidence/usage.md (commands, config shapes, gaps), evidence/compatibility-offline.md (offline vs hardware/cloud unknowns), evidence/README.md (index) and evidence/snapshot/ (fixed candidate manifest).

## Next steps

- Independent acceptance agent reruns `make verify-m0` in an isolated workspace on this fixed candidate and checks each A01-A28 mapping against the implemented tests.
- Specification owner/referee confirms approval status, resolves open decisions and rules on UDP no-authentication risks.
- Obtain explicit authorization + budget for L01-L03 and device/network authorization + restore plan for H01-H06; implement the authorized workflows behind the existing gated entry points, then run them.
- Do not deploy publicly or implement M1 before M0 acceptance and a public-network UDP security decision.

## Re-run

```bash
make verify-m0
```

Live/hardware entries are `make verify-m0-live` and `make verify-m0-hardware`; without an authorized `local/` config they exit non-zero and report BLOCKED.
