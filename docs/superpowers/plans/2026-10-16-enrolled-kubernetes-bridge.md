# Kubernetes enrolled management bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. 同一完整Task内多个小提交；不在切片间停下。

**Goal:** 为正式 enrolled launcher 提供固定 Kubernetes exec 字节通道，直接供已有 Destination/bootstrap 消费。

**Architecture:** 静态私有API配置和有界点查固定候选，原始HTTP升级到唯一SPDY/v4。客户端拥有原TCP、Serve、四流和关闭join，原管理TLS/Backend才授权业务。

**Tech Stack:** Go1.25.6、现有Kubernetes v0.35.3/moby spdystream v0.5.0，无依赖升级。

**Spec:** docs/superpowers/specs/2026-10-16-enrolled-kubernetes-bridge-design.md

## Global Constraints

- 仅固定绝对launcher bridge，无shell/任意命令/endpoint/callback，constructor不联网，复制/零值拒绝。
- Namespace固定；MaxActive1..16/RequestTimeout>0..5s；metadata/config256KiB、upgrade headers64KiB、stderr4KiB/status16KiB；无按已有N的对象/ticker/扫描。
- 私有静态凭证/TLS；拒绝环境代理、插件、文件加载器、transport注入/redirect/重试/协议fallback；四流真实FIN和所有原IO/Serve/Close join。
- 点查仅诊断，不替代Root-signed birth/TLS/Backend fence，不将NodeBootID当Runtime BootID，不声称exec UID precondition。
- 仅apply_patch；不碰Sentinel/外部环境/依赖升级/push/deploy/CLOSED OWN；无native运行；Root-only exactpaths/expectedHEAD/common→OWN锁小提交。
- 一个fresh author/一个fresh完整review，同pair scoped修复，无nestedagents；实际GOOS/CGO/race全17GoList输入前后filename/SHA，独立epoch不重放旧已验收矩阵。

## Review Focus

1. Dial短ctx成功返回后取消，不得杀死已移交连接；建立取消必须真实关闭并join。
2. deadline/关闭与并发Read/Write/FIN的锁序、退款时机、SDK后台Serve无join陷阱。
3. malformed/oversized/redirect/协议回退不导致无限分配、隐式重发或凭证外发。
4. 原始TCP buffered HTTP字节不能在切SPDY时丢失，stdout EOF不能掩盖非零status/stderr错误。
5. copied candidate不能授权native行为，Node/Pod替换不能把HTTP成功当安全，cleanup纯提取不得改变其预算或现有实际请求规则。

### Task 1: Complete owned Kubernetes bridge transport

**Create:** internal/runtime/kubernetes/enrolled_bridge.go（public DTO/client/slots/pointchecks），enrolled_bridge_upgrade.go（固定原始HTTP/TLS/SPDY建立），enrolled_bridge_conn.go（四流/IO/deadline/close），enrolled_bridge_test.go（真实wirefixture/新selectors），static_api_config.go（如确需DRY的纯静态配置快照）；scripts/test-enrolled-bridge.sh（本OWN actual17输入collector/限定host/race/vet）。按职责可拆独立test文件。

**Modify narrowly:** internal/runtime/kubernetes/task_runtime_cleanup_transport.go 仅复用纯static snapshot，保留所有原HTTP/actualdial/body/预算行为；新专门回归覆盖提取。禁止改旧exec/main/原producer。

**Interfaces:** NewEnrolledBridgeClient(EnrolledBridgeOptions)(*EnrolledBridgeClient,error), (*EnrolledBridgeClient).Dial(context.Context,EnrolledBridgeTarget)(net.Conn,error), Close()error；options/target/spec caps见spec。没有注册global factory或接收外部Dial callback。

- [ ] Step1 先读实际Pod profile/controltransport/cleanup和installed spdystream的Serve/Close/Wait。报告具体冲突，Root ruling后执行；未获START-GO前只读。
- [ ] Step2 最小新selector RED→实现static snapshot/client/2GET完整tuple校验，host/race/affectedvet及新cleanup提取回归通过，Root及时小提交。
- [ ] Step3 新wire selector RED→固定POST upgrade/SPDY四流/二进制/半关闭/原短ctx不传染，host/race/affectedvet通过，Root及时小提交。
- [ ] Step4 新故障selectors RED→补取消/deadline/额度/实际join/oversize/redirect与错协议，验证各规定场景，Root小提交，不重放已GREEN矩阵。
- [ ] Step5 author冻结全部源码SHA/各epoch原日志/报告；Root唯一fresh整Taskreview，限定修复、tracked验收，继续master真实创建/发布编排。

## Root preflight and rulings

| Pair/task | Producer → consumer | Checked / ruling |
| --- | --- | --- |
| profile predecessor → Task1 | shared Inspect/canonical config → bridge pointcheck | 原profile先accept才START-GO；不改原validator，不读前OWN产物。 |
| Step2 → Step3 | fixed origin/static TLS/target → upgrade | 只静态bytes，无可选URL/command、无SDK重试；网络动作具体实现，非成功factory。 |
| Step3 → Step4 | TCP/Serve/streams → Close/cancel | 直接拥有moby Serve可join，不能用remotecommand StreamWithContext返回充数；真实FIN/error/status必须测。 |
| Task1内部 | cleanup configure → pure snapshot | 原配置拒绝规则保持，原active预算/dialtracker不共享；新限定回归保护行为。 |
| Task1 → future composer | copied target/byteconn → Root cert/Backend authority | 本Task不发业务授权、不Create、不退款持久slot、不解决64node publication组合预算。 |

Ruling: 用户已批准完整方案并要求“继续，不要停”，这是其中真实driver管理通道实现细化；无需重复审批。选择直接固定SPDY兼容目标并实际join，相比SDK多出少量协议接线，后续真实集群兼容性如不符须明确修改本合同并复审。未部署/未切main/未解决Createunknown与长期证书维护，不能报告为这些完成。
