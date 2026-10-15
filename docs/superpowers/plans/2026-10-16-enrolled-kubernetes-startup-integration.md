# Enrolled Kubernetes startup integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. 一个完整Task、及时小提交，持续执行。

**Goal:** 正式PID1接受并安全收紧真实EmptyDir初始权限，真实生产WebSocketclient→正式bridge→TLS publication闭环。

**Architecture:** root持有FD先收紧control再检查唯一socket/新建配置；源级复用原fixture服务，新native只将实际default tmpfs和固定exec bridge接入原认证链，不改权限模型。

**Tech Stack:** Go1.25.6/Linuxarm64、现有Kubernetes/Gorilla/Unix依赖，无升级。

**Spec:** docs/superpowers/specs/2026-10-16-enrolled-kubernetes-startup-integration-design.md

## Global Constraints

- 只root/PID1；root/run祖先不放宽，control root:root exact0700/0755/0777/01777→原FD先0700再子项检查；旧birth数据不覆盖/删除。
- 原五volume/authority/生产cap/UIDGID/BootstrapPID1和TLS校验保持；不加生产cap/init/shell/任意program/Dial成功factory。
- 新native必须实际tmpfs size1m不设mode→正式CLI，原Kube bridge client→实际正式bridge child→原TLS ready验证，所有原IO/Wait/Serve/Closejoin。
- 新OWN fresh产物、actual17环境匹配/prepost filenamesSHA/每archive成员/ELFscript独立绑定；不重放旧矩阵，不碰Sentinel/CLOSEDOWN/外部环境/升级/push/deploy。
- 只apply_patch；Root-only exactpaths/HEAD/common→OPEN OWN锁小提交；唯一fresh author/reviewer，同pair修复，无nested。
- 每作者1/总2fixture上限、原512MiB/2CPU/256PID/no-network/private/nohostPID/readonlyroot/NNP；唯一setupcap例外SYS_CHROOT在两个固定helper正式exec前连同ADMIN/PTRACE从bounding等全部丢弃并核验，生产四cap不变；额外仅private exactcontrol tmpfs≤1MiB，加原/run≤1MiB。实际run须Root具体批准。

## Review Focus

1. 不能放宽root/run或旧serve边界；worldwritable初始control仅rootFD收紧，不在用户已运行后冒认fresh。
2. 特殊mode/owner/symlink/stale/socket替换被拒；错误路径不得覆盖或删除旧birth数据。
3. native RED来自正式CLI特定拒绝，GREEN确实用无mode独立tmpfs；不回到手工0755弱fixture。
4. exec server必须运行本OWN正式bridge ELF，原namespace FD/child身份、丢setupcaps和TLS认证真实，不能靠JSON/factory成功。
5. hijackedWS/copy/childWait/PID1/observer/clock/etcd实际全部join；未知/timeout不作成功或提前退款。

### Task 1: Safe EmptyDir bootstrap and actual Kubernetes bridge integration

**Modify:** cmd/sandbox-launcher/provision_linux.go；按需新增纯mode策略文件/专门测试；internal/runtime/controlrunner/enrolled_launcher_native_linux_test.go及enrolled_profile_native_linux_test.go仅最窄test-only helper抽取，新selector由新文件拥有，旧selectors默认不变。

首个真实组合失败后新增最小范围：internal/runtime/kubernetes/enrolled_bridge_conn.go、internal/runtime/controltransport/destination.go及专用真实TLS回归，解决TLS CloseWrite的底层过期写deadline误杀响应；新startup script裸断言显式失败出口及无Docker回归。原失败和wrapper0/attach1矛盾保留，不重开CLOSED任务；只限定复验实际受影响的旧selectors。

**Create:** internal/runtime/controlrunner/enrolled_kubernetes_startup_native_linux_test.go；scripts/test-enrolled-kubernetes-startup.sh；可另拆test-only固定namespace exec/WS relay helper，避免复制整套服务。

**Interfaces:** 原生产client `NewEnrolledBridgeClient`, `Dial(ctx,target)(net.Conn,error)`；原BuildEnrolledPlainPod、Destination、publication verifier。新增全是私有有限testhelper/固定mode策略，不改原public authority接口。若原fixture无可替换Dial点，作者先报具体最小源级ruling，不复制完整fixture。

- [x] Step1 READONLY预检实际fixture接口/路径/原Wait；bridge已937f018范围验收，同HEAD为immutableBASE/START-GO。
- [x] Step2 新mode策略0777/01777真实RED；独立默认tmpfs实际01777，正式CLI拒绝invalid fresh control directory，原child/attach/container均exit1，原服务退出与exact容器清理齐全；原输入归档保留，不单独提交红态。
- [x] Step3 ee0bc98：rootFD先0700并Fstat后检查子项，有限mode回归host/race及Linuxvet通过；只提交3个连贯产品/回归文件，不改共享profile/管理权限。
- [ ] Step4 首次真实组合失败已定位TLS写期限和旧Bash断言问题；新真实TLS/方向性回归RED→最小修复GREEN，脚本显式失败出口验证，再freshbuild nativeGREEN经生产WSclient和正式bridge；原verify/IO/Wait/join/精确清理/all17及ELF绑定，及时提交。不加可选负例拖延主链。
- [ ] Step5 源码三次小提交ee0bc98/1deb965/ddd2cd3，新native green-bridge-r1真实通过；唯一完整审查C0/I1/M0要求补有限publication的底层transport终态顺序。同pair R1在原boundedctx内先严格TLS EOF、再原raw n0/EOF后成功Close，受控gate新RED→GREEN/取消验证及限定fresh native，随后原reviewer仅复审I1和直接新增破坏、tracked验收；继续真正create/quota/manager。

## Preflight and rulings

| Pair/task | Producer→consumer | Finding/ruling |
| --- | --- | --- |
| profile→startup | 五volumeMemoryEmptyDir→CLI | 前0755假设错；官方kubelet setup/mount和实际内核mode必须在新Task修复，旧证据历史范围不重标。 |
| bridge→startup | ownedv5byteconn→真实Bridge/TLS | 前Task只hostwire；本Task真实PID1中间bridge exec，不冒称真实CRI。前置accept才Go写/build。 |
| Step2→Step3 | 真实初始modeRED→FD收紧 | cap不增加，先tighten后enumerate，所有旧文件保留。 |
| Step3→Step4 | 正式provision→enrollment/WS | 不用kernel/authority成功factory，固定exec及实际namespaceFD/Wait链。 |
| Task内部 | 已有native服务→新fixture | 允许最窄test-only mode/exectransport seam；不提供生产可选program，具体接口预检后Root裁决。 |

Ruling: master持续授权下的真实兼容修复与组合验证，不额外停止审批。额外control tmpfs仅为准确复现MemoryEmptyDir，仍在512MiB总memory内；root/run/hostmount/生产caps均不放宽。实际setns要求SYS_CHROOT，Root纠正初始setupcap假设：仅新fixture新增此cap，原PID1 helper startup-only与bridge helper正式exec前必须真实drop bounding/ambient并核验只剩原四cap；不增加broker或绕过原namespaceFD。详见spec权限纠正段和官方setns规则。
