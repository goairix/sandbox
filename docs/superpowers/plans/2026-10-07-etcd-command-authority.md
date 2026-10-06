# Native command authority Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 为effect intent提供配置化issuer与永久不可变CertificateID/digest依赖，无idle-N刷新。

**Architecture:** Task1 operatorDI/pinnedverifier/recordcodec；Task2 bounded native CAS注册与fixedpoint历史查询。注册未知不开始物理effect，后续Stage effect+originalcap消费者另有下一单元。

**Tech Stack:** existingnative etcd3.6.14 client/owned3.6.15fixture、controlprotocol、Go标准库、testify。

**Spec:** `docs/superpowers/specs/2026-10-07-etcd-command-authority-design.md`；连续授权整体phases1–5下必需依赖，逐验证小提交。

## Global Constraints

- Provider exacttyped `Certificate(ctx)([]byte,error)`、`SignStart(ctx,certificateDigest string,claims controlprotocol.ExecStartTicketClaims)([]byte,error)`；Options.ExecIssuer optionalnil、typednil拒绝；Backend不持有root/delegateprivatekey；仅operatorDI，immutable/context-honoring/concurrency-safe。
- 非nilprovider要求合法PublicationTrust+Clock，copied1–2pinnedroots/exactbinding，与既有AuthorityID==Identity.RuntimeID规则相同；startup不调用provider/新增clock观察/注册/bootstrap/后台刷新。
- immutableCommandIssuerRecord Version1、Namespace canonicalRoot512B、RestoreEpoch validDomainSegment、certIDcanonicalnonnilUUID、digest64lowerhex；Certificate opaque合法UTF8 JSON<=4096B，snapshotDigest相等；record<=8192B，strictrequiredsnake_case/unknown/duplicate/null/trailing；KVLease0、CRev>0、ModRev==CRev，historicalmetadata不是crypto/active/capability。
- key command-issuers/UUID固定point，无Range/scan/Grant/KeepAlive/Revoke；注册freshroot验证与canonicalidentity生成record；不调用SignStart、不发命令、不删除/覆盖cert，不通过publicentry重建authority。
- 注册CAS base4+keyCRev0，Then永久Put，Else默认linearizableidentity/restore/registry3points；任何RPCunknown不猜失败/不换ID；最终identity-fencedexactread确认实际immutablebody+firstrevision，absent=>unknown，同bodyreplay保原CRev，不同bodyconflict，corruption不吞成conflict。
- 无新dependency/Manager/configimageproduction切换、不读.env/生产凭证、不push/merge/deploy；保留Sentinel无关编辑。native测试仅controller-ownedfreshfixture，不复用历史端口/未拥有集群；worker teardown boundedjoin。验证自审即本任务commit，独立spec+qualitygate后下一task。

## Review Focus

1. typednil/roots或optionsalias、provider意外startup调用破坏optionalmetadata-only行为（Task1）。
2. 同UUID不同合法wire/key与旧restore记录不被覆盖；同bodyreplay不改变首次CRev（Task2）。
3. committed-replyloss/迟到CAS/最后absent不宣称已失败，不生成physicalcap（Task2）。
4. foreigncluster/identity/restore、emptybranch stale comparison及异常response shape不能回metadata成功（Task2）。
5. copiedwire/strictouterlimits/leased或mutatedrecord与历史过期wire不绕freshmutator；load仍仅structural（Task1/2）。

## Task 1：operator issuer DI 与 strict registry codec

**Files:** Create `internal/storage/state/etcd/command_authority.go`, `command_authority_test.go`, `command_issuer_records.go`, `command_issuer_records_test.go`；Modify `client.go` 仅Options/Backend字段及factory validation/New保存。

**Interfaces:** produces ExecCommandIssuer exactspecinterface；Options.ExecIssuer；Backend.execIssuer/execVerifier；privateexecAuthority(Options)(*controlprotocol.ManagementVerifier,error)。CommandIssuerRecord{Version uint32;Namespace,RestoreEpoch,CertificateID,CertificateDigest string;Certificate json.RawMessage}与Validate()error；CommandIssuerEntry{Record CommandIssuerRecord;Revision int64}；Namespace.commandIssuerKey(string)(string,error)，专用encodeCommandIssuerRecord(record)(string,error)、decodeCommandIssuerRecord(kv,*record)error，freshcrypto由Task2消费现有ManagementVerifier而非codec。

- [ ] TDD `TestExecCommandAuthorityConfiguration` optionalnil/typednil/缺trustclock/wrongauthority/roots/copy/构造不callprovider；`TestCommandIssuerRecord` normal/copy/strictmissingunknownnulltrailing/UUIDhashnamespaceepoch/wireinvalidUTF8JSONover4096/recordbounded/LeaseCRevModRev/key生成。记录raw历史载荷非freshidentity，opaqueinner不要假装Verify。
- [ ] focused `go test ./internal/storage/state/etcd -run 'TestExecCommandAuthority|TestCommandIssuerRecord' -count=1` 实际behaviorRED，最小实现，不加runtime/issuer service/registryRPC。
- [ ] focusedGREEN+targetedrace、packagevet/gofmt/diffcheck；本task纯配置/codec不重复全nativefaultsuite，metadata-only既有配置测试覆盖一次。自审仅stage本task5files即commit `feat(etcd): configure immutable exec issuer authority`；完整报告、独立spec+qualityreview。

## Task 2：native immutable certificate registry

**Files:** Create `internal/storage/state/etcd/command_issuer.go`, `command_issuer_test.go`；允许另建 `command_issuer_fault_test.go` 专门workerjoin/RPC fault，不改Task1interfaces/其他metadata协议。

**Interfaces:** `(*Backend).RegisterExecIssuer(context.Context)(*CommandIssuerEntry,error)`、`(*Backend).LoadExecIssuer(context.Context,string)(*CommandIssuerEntry,error)`。producerTask1types/factory/codec；consumes ObservePublicationClock/freshManagementVerifier/identity-fencedreadDomain/baseComparisons/validatedresponsehelpers。不调用ExecIssuer.SignStart，futureeffectconsumer须freshverify+exactregistryfences。

- [ ] TDD `TestExecIssuerRegistry` nativefreshscope/canonicalwire/firstCRev/Lease0/replay/copy/expiredhistorical/zeroGrant；`TestExecIssuerRegistryRejects` freshwrongrole/root/binding/time/nilctx/provider/strictresponse/recordlease/mutation/epoch/identity拒绝与无wrongwrite；`TestExecIssuerRegistryCompetition` validdifferentkeys同UUID最多一body；`TestExecIssuerRegistryReplyLoss` 实际committedreplyloss后Load/replay不重写；`TestExecIssuerRegistryDelayedCAS` 实际before-server完整Txn延迟与另一body竞争，最终不覆盖，hook worker必须boundedjoin。
- [ ] controller提供新唯一ownedmanualfixture endpoints/project/containers，worker用该fixture跑focused实behaviorRED（不以missingAPI编译错代替）。实现精确流程/bounds/shape/unknown，所有authority读取固定linearpoint，无新Lease；nativefailpathctx/release/join资源先于cleanup。
- [ ] focusedGREEN/targetednative-race、packagevet/gofmt/diffcheck；记录真实cluster/version/top-levelSKIP是否0、CAS/point/Grant0/wiresamples，自审即commit `feat(etcd): register immutable command issuer certificates`。独立spec+qualityreview后controller最终gates。
- [ ] controllersource冻结后freshownedscript完整nativefault-race、配置manualfixture全仓test/vet/build必要并行checks，真实通过/SKIP/expectedfaultwarnings与fixtureownership cleanup核对；完整base..head整分支finalreview、一次finalfixwave/scoped复核、逐项rulings/declined保存Git，继续originalcap effect intent计划。

## Self-review

两task共享client.go中的新字段和codec，Task1提供完整签名，Task2只消费不重写；Task1tests与schema/optionalcfg规则一致，Task2测试真正nativeCAS+replyloss并有join。所有ReviewFocus分配具体测试，readOnly metadata loader不fresh且无cap、Registerfreshcrypto但不targetactivation，两者没有互相冒充。少量全局issuer永久retention成本显式不由B_live/N掩盖，后续safeGC仍必做。没有含糊promise把CAS unknown或年龄变成executionabort。
