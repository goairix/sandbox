# Sentinel 恢复核验可达性分类 Implementation Plan

> REQUIRED SUB-SKILL: test-driven-development；原地执行、不访问集群。

**Goal:** 让恢复包装器分辨真正端点不可达与“已连接但无法核验”，避免将较新却暂不一致的成员当失联忽略，然后恢复旧主。

仅新建 `internal/redisbootstrap/challenge_contact.go`、`challenge_contact_test.go`。既有Fetch函数与协议不变；新固定端点入口仍复用严格读取/验证。

```go
type IdentityContactState int
const ( ContactRejected IdentityContactState=iota; ContactUnreachable; ContactVerified )
func FetchIdentityProofContact(context.Context,ClusterState,Member,ed25519.PublicKey,IdentityChallenge,*VolumeIdentity,*AuthenticatedEndpoint) (IdentityProof,IdentityContactState,error)
```

public固定validatedDNS:18080/v1/identity，不暴露URL/client/provider。newIdentityHTTPClient仅属于本次请求，wrap其真实DialContext，记录实际是否尝试连接以及是否成功取得TCP连接（atomic.Bool，防client异步Dial race）；无代理/redirect/keepalive/retry，3s预算/已有body限额保持。private helper可接受httptestURL用于actualTCP。

输入invalid（cluster/member/key/challenge/expected/current或nilctx）返回Rejected/零proof/常量错误，不连接；live必须有Configured expected和当前已认证相同member/run_id。已连接后任何503/timeout/invalidJSON/wrongsignature/marker/replay均Rejected，恢复调用者须阻断或重试，不能当不可达跳过。只有实际Dial尝试但未建立TCP且caller context仍有效的错误才Unreachable；没有实际Dial不属于Unreachable。caller cancel/deadline保留ctx.Err并Rejected，不把主动停止当节点失联。成功仅Verified+真实已验证proof；error不含URL/密码/原网络诊断。

状态不是对全局网络的证明；Unreachable只是本次固定endpoint连接失败，调用者仍需两个可信保留状态及原主角色。该函数不决定主、不启动Redis，不把Inventory视为live角色证据，也不加入新proof purpose。

- [x] actualTCP closedendpoint、503handler、validsignedresponse测试stub行为RED→GREEN。
- [x] nilctx/invalidarguments无request，connect后headers超时与caller取消Rejected；tamper与错误markerRejected，错误redaction。
- [x] focused/full/race/vet/lint与先规格后质量独立复审；fullwrapper状态决策/真实HA另行验收。

## 当前执行证据

初始 stub 在真实已关闭 TCP endpoint 与有效签名响应两项行为测试产生预期 RED；实现后 focused PASS。补充 invalid arguments 无连接、503、错误签名、重放 nonce、有效签名但错误 marker、主动取消及已建立连接的 header 超时，`go test ./internal/redisbootstrap -run TestIdentityContact -count=1 -timeout=20s` PASS 2.747s；race focused PASS 4.735s。初版挂起服务 fixture 的清理等待不能靠 request context，已改为测试专属 release channel，并结束精确核对的两个挂起测试进程。规格/质量复审及整体验证仍待完成；不是完整冷恢复或 HA 通过声明。

独立 SPEC PASS；QUALITY 捕获真实异步 dial 未完成不能当失败的 Important 问题，实际 TCP 成功但 ConnectDone 阻塞的回归产生预期 RED（3.01s，旧实现 Unreachable）。现显式记录完成失败，未完成保持Rejected；invalid 输入另断言零 ConnectStart。修复 focused PASS 5.794s、父任务 race PASS6.664s；独立 QUALITY 重审 PASS（其 isolated focused5.753s/race6.911s），无剩余问题。整体验证仍须等当前启动接线稳定后执行。

最终独立 SPEC/QUALITY 均 PASS；完整接线及真实运行验收记录见 [2026-09-14验收报告](../../testing/2026-09-14-built-in-sentinel-production-startup-results.md)。上文阶段性待复审描述保留为历史执行记录。
