# 配置化 command issuer 与不可变证书注册

在已批准整体 phases1–5 下连续实施。前置 exec descriptor/start 协议已完整复核；本单元补齐 effect admission 需要的全局 issuer 依赖：operator DI signer、pinned verifier 和永久 immutable CertificateID->canonical certificate/digest 记录。下一单元才组合 original OperationCapability 与 Stage effect intent；本单元不调用 runtime、不返回命令能力、不激活 target。

## 选择与范围

同 CertificateID 不可绑定另一份 wire/digest/key；注册是全局 deployment issuer 的 metadata，不为每个 idle sandbox 添加 key、Lease 或刷新循环。采用 identity-fenced absent-or-exact-existing CAS，比给纯证书注册另加 Stage 更小：注册未知不会引发物理 effect，永久唯一 key 已排除两个不同 body 同时成功。只有未来 effect intent 的未知提交需要 Stage 仲裁。记录暂永久保留，不能在本单元自动删除过期证书；证书数量/字节按实际 issuer rotation 统计，不宣称 GC 或总容量完成。

## Operator 配置边界

公开接口：
```go
type ExecCommandIssuer interface {
 Certificate(context.Context) ([]byte,error)
 SignStart(context.Context,string,controlprotocol.ExecStartTicketClaims) ([]byte,error)
}
```
SignStart 第二参数是要求使用的 canonical certificate digest；可信 provider 必须选择该 exact identity 并仅签固定 start purpose。provider 是 operator 配置的 immutable、context-honoring、concurrency-safe 依赖，允许内部按真实管理需求轮转；请求参数不能选择 signer/roots/now。Backend 不持有 root 或 delegate privatekey；测试内存 signer 不是生产 key-custody 实现。

Options 增加 `ExecIssuer ExecCommandIssuer`；Backend 私有 `execIssuer ExecCommandIssuer`, `execVerifier *controlprotocol.ManagementVerifier`。nil ExecIssuer 允许旧 metadata-only 用法；typednil 拒绝。配置非nil时必须配置合法 PublicationTrust+Clock（复用既有一致规则、AuthorityID==Identity.RuntimeID、copied1–2roots、exact Namespace/AuthorityID/Target/RestoreEpoch），构建新的 ManagementVerifier。New/validateOptions 不调用 provider Certificate/SignStart、不 Observe 额外时钟、不启动刷新/注册/bootstrap。后续 actual command demand 才调用。私有 factory `execAuthority(Options)(*controlprotocol.ManagementVerifier,error)`，错误 wrapping ErrInvalidConfiguration。保持既有 TLS/identity/clock 校验与 metadata-only 行为。

## 永久注册记录

`CommandIssuerRecord` required snake_case fields：Version uint32=1；Namespace,RestoreEpoch,CertificateID,CertificateDigest string；Certificate json.RawMessage。namespace canonical Root（512B）、epoch validDomainSegment、CertificateID canonical nonnil UUID、digest64 lowercasehex；Certificate 为非空合法UTF8 JSON、<=4096B、其 snapshotDigest==CertificateDigest。外层record<=8192B。RawMessage 仅 structural opaque 历史载荷，Validate 不声称 crypto/fresh/active。所有真正注册 mutation 使用 fresh pinned ManagementVerifier 验证证书，记录从返回 identity getters 生成，忽略 request 自述字段。编码/解码复用 strictPreparationMetadata 的 required/unknown/duplicate/null/trailing 规则及 decodeDomainRecord；record KV Lease=0，CreateRevision>0 且 ModRevision==CreateRevision。证书 JSON 内部签名/严格schema由 fresh protocol verifier 验证，历史 loader 不能代替它。

key = namespace.Key("command-issuers",canonicalCertificateID)，固定一个point，无Range/全局扫描。namespace.RestoreEpoch 与当前backend一致；旧epoch记录拒绝作为当前 registry 证据，新 epoch 由 CA 使用全新 UUID。记录不可覆盖/删除；灾备 quarantine/长期GC/生产CA唯一ID策略仍后续必须实现。

public `CommandIssuerEntry {Record CommandIssuerRecord; Revision int64}` 是复制的 structural metadata；不含privatekey、不含CommandIssuerIdentity/PreparedCommand/transport权限。loader每次返回新certificate字节，输入/output/缓存不共享mutablebytes。未来 effect consumer 需自行fresh crypto认证、exact canonical wire/value+ModRevision+Lease0 fence，不接受caller伪造entry。

## 注册与查询

`(*Backend).RegisterExecIssuer(context.Context)(*CommandIssuerEntry,error)`：nil/canceledctx拒绝；缺provider/verifier/Clock配置ErrInvalidConfiguration。bounded request context 调 provider.Certificate、立刻copybytes；调用可信 observePublicationClock，再fresh VerifyCommandIssuerCertificate；由identity生成canonicalrecord/key，preflight bounds before任何写。不调用SignStart（该方法留给下一个 effect 单元）、不Grant/KeepAlive/Revoke。

Txn If base4 identity/restore comparisons + CreateRevision(key)==0；Then单Put永久record；Else默认linearizable点读取identity/restore/registry，保证同Txn比较不走empty/read-only stale classification。所有RPC用bounded request context；失败ErrOutcomeUnknown（已发送RPC错误不证明未写），缺/错header、cluster/revision/responsekind/cardinality failclosed，identity/restore不符明确ErrIdentityMismatch。成功或false后都通过 identity-fenced point loader重新读取并验证 immutable exact canonical record，返回真实firstRevision；同body replay成功且不改ModRevision/Lease，不同body同UUID ErrConflict；任何 corrupt immutable envelope 先ErrCorruptRecord，不把它当普通collision。若最终read absent只返回ErrOutcomeUnknown，不宣称失败、不删除/换ID；迟到原CAS仍可能提交。unknown后同exactbody retry是幂等注册，物理execution还不存在。

`(*Backend).LoadExecIssuer(context.Context,string)(*CommandIssuerEntry,error)`：只接受canonicalnonnilUUID，读一个registrypoint（新私有fixedpointreader携带同样base4 identity/restore fence，并保留完整Txn header/shape验证；不改旧readDomain协议）；全absent返回nil,nil，仅是快照缺失，不是注册失败或执行终态；拒绝leased/mutated/partial/错key/namespace/restore。没有originalrevision参数的历史loader不宣称检测授权raw writer删除后coherent重建同body；未来effect消费者必须保存并比较originalregistry ModRevision，已有cap不得采用重建后的新revision。无需ExecIssuer/clock，过期cert仍可structural历史读取，不能从它签发或重建能力。query result copybytes；不读wire来安装trust。

## 验收与下一依赖

真实三成员 ownedfixture：normal/replay sameCRev、并发sameUUIDdifferentvalidcert最多一个body、root/role/scope/time invalid无write、RPCcommitted-replyloss恢复、迟到CAS与另一body竞争不覆盖、identity/restorechanged failclosed、strictshape/leased/mutable/copy界限。native hooks/worker必须cancel/release并boundedjoin后恢复，不能新的earlyFatal泄漏。固定point计数与0Grant证明局部成本，字节样本不证明真实10k/100k/1M或SLO。无fixture skip不得当native通过。

随后 effect plan必须配置此provider、fresh认证其返回ticket、将 registry.Value/ModRevision/Lease0 与 original五controlfences+guard/token/receipt/Lease/CRev、parent/lost/deadline与boundedUTC一并比较，再写 immutable effectintent beforetransport。pure start30秒上限不可替代原Lease剩余时间。target activecert/gate/BootID/actualpayload/journal、acceptedrenew、terminal/End/drain、GC、Manager/Redis替换仍全数必做。

Self-review: SignStart被声明但本单元不调用，便于下一effect消费者拿固定typedAPI，未引入rawroot signer。注册只固定全局issuer metadata，CAS未知幂等不构造executioncap，outerstrict与opaqueinner语义分开；metadata-only legacyconfig仍合法。RootDigest来自freshidentity规范wire，空snapshot读取不猜失败；永久无Lease记录originalrevision由后续effect消费者固定，raw writer coherent重建不能被已有cap当作原fence；历史loader本身只证明当前结构。记录数随issuerrotations非idleN，不隐藏永久retention成本。
