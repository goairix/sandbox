# Linux Node Observer Physical Producer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 实现并用真实Linux PID/cgroup验证一个独立可信物理终止观察者，为后续安全owner释放提供具名来源。

**Architecture:** root-certified node observer通过UDS内核peer和runtime固定签名挑战登记真实PID1，保留pidfd/namespace/cgroup句柄；独立验证固定etcd权限与精确Kubernetes no-restart前置条件，单次终止、实际等待、持久签名查询。一个完整功能Task，小步验证提交，一次fresh独立review。

**Tech Stack:** Go1.25.6、现有x/sys/unix/client-go/etcd3.6.15/Ed25519、Linux arm64/amd64；无新依赖。

**Spec:** docs/superpowers/specs/2026-10-15-node-observer-physical-termination-design.md

## Global Constraints

- 独立producer实际enroll/signal/join/journal/query；不实现publication/etcd写端/release/main/manifests或上线，不把组件完成称生产迁移完成。
- 原协议/角色/validator不放宽，无导出任意签名/physical-success callback/PID或cgroup路径授权；共享strict codec，不import state/etcd。
- 固定59comparisons+1read=60nodes，native请求/两分支响应≤256KiB；cap intent16384、frame16384、slot32768、两代journal≤64MiB payload。
- slots1..1024/active1..4/RPC≤5s/termination≤30s；unknown永久占slot，一次signal，actual IO/Close/join后才释放；不可把ctx cancel当退出。
- 固定/proc和/sys/fs/cgroup，不接受Options重定向物理事实；实际peer/pidfd/starttime/ns/dev/inode/root/boot验证，保留namespace与cgroupanchors。
- 不触碰当前etcd local finalization文件或已验收CLOSED OWN；不碰/读取无关Sentinel计划；无online Redis/K8s、secret/env/fullinspect输出、push/deploy。
- 只apply_patch写源码/测试/文档/报告/格式；无cat/heredoc/Python内容写入和gofmt-w；工具生成raw/JSON/binary/sourcearchive/diff/brief例外。
- 每次Git写先common锁再OPEN OWN锁fcntl LOCK_EX；Root窗口+expected HEAD/branch/空index/精确path/stagedcheck/postempty；小步连贯提交。
- 最多两个总native fixtures；当前local-sync占一个，observer先与Root协调。Linux能力缺失要报告真实不可用，不用mock或SKIP当物理验收。

## Review Focus

1. UDS转发/peer替换/用户读取runtimekey及nonce重放，不能把签名当内核peer身份。
2. PID复用、节点boot/cgroup/ns改变、后代逃逸或D状态，保持unknown且真实占slot。
3. etcd lease/claim丢失与DELETE未知/Never约束变化，已发请求不重发，不签伪终态。
4. journal fsync/rename/Close断点及restart丢anchors，原unknown不改写，terminal可只读查询。
5. 固定slot/FD/frame/wire达到上限、并发Close/慢peer，不泄露句柄、预算或私钥。

---

### Task 1: Real kernel-backed node observer producer

**Files:** Spec Precise file boundaries全部新producer/protocol/transport/command文件；new focused tests同目录，controlrunner/node_observer_native_linux_test.go仅fixture，scripts/test-node-observer.sh。禁止当前etcd路径/旧API泛化重构；额外接点先Root ruling。

**Interfaces:** nodeobserver.New(Options)(*Observer,error)、Observer.Serve(ctx,*net.UnixListener,net.Listener)error与Close()error；Options精确字段和固定协议OpenNodeEnrollment/Session.Enroll/TerminationDestination.Terminate/Query签名见Spec60/68；shared Encode/DecodeTaskRuntimeTerminationIntent见Spec59；固定签名/verification角色和claims见Spec46/61–67。

- [ ] **Step1 RED+strict wire/authority。** 新TestNodeObserverProtocol验证role/domain/canonical fields、max16384/duplicate/null拒绝及固定key/hash；TestNodeObserverAuthority验证真实TLS etcd claim/Lease/header/59+1/256KiB，错误native数据拒绝无signal。先记录RED，再实现共享codec、新role/tickets及concrete clients/guards。GREEN后小提交，不先建大型框架。
- [ ] **Step2 enrollment/journal。** TestNodeObserverEnrollment验证actual SO_PEERCRED+nonce/runtimecert关联、固定node/Pod profile/root/boot、真实pidfd/ns/cgroupanchors；TestNodeObserverJournal验证固定槽、canonical≤32768、双代fsync/crash、满/损坏拒绝。实现注册与有限owner/Close生命周期，focused GREEN后提交。
- [ ] **Step3 physical terminate/query。** TestNodeObserverPhysicalNative真实private PID1及forked descendants，deleting exactUID+Never前置，actual pidfd SIGKILL、death+originalcgroup empty、durable signed terminal、restart Query原bytes；单次signal/lostACK/query不执行、blockedkernel/join/cancel保留unknown。Linuxfixture必须真实验证权限与cgroup，缺失向Root报告；不得mock替代。GREEN后提交。
- [ ] **Step4 refusal/resource/lifecycle。** 真实peer转发/PID/ns/cgroup/boot替换、claim/restore丢失、unknownDELETE、journal断点、FD/slot/wire上界、backpressure和并发Close验证ReviewFocus五类；保留实际child Wait、exactlabels/ownedcgroup cleanup、source/binary SHA。源码改动仅跑新增受影响case，及时提交。
- [ ] **Step5 complete evidence+self-review。** 新增hostrace、affectedvet、Linux compile、gofmt-l/bash-n/diffcheck；不重跑accepted矩阵。写OWN/task-1-report.md，列小commits、RED/GREEN各epoch命令/结果/原始Wait与cleanup、key/node/wire和FD/allocation表、明确production integration/rollout pending。Root仅派一次fresh taskreview，限定fix轮次同author/reviewer，验收后继续publication/termination/release而不结束master。

Plan执行由用户既有master批准与多次continue授权；不重复询问是否继续。受限privileged Linuxfixture仅操作具名测试资源，若不能证明确切资源边界则停止该fixture并报告，继续可独立源码工作。
