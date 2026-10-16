# Etcd Large Download Implementation Plan

> For agentic workers: REQUIRED SUB-SKILL subagent-driven-development。一个完整有界下载Task，唯一fresh author/reviewer，小步验证提交。

**Goal:** 配置后的显式etcd HTTP下载最多2GiB，内存固定、磁盘有总预算，原终态之后才成功交付。

**Architecture:** 普通pinned launcher binary-download child→一个原Exec→private bounded API spool→verified terminal/Seal→HTTP Content-Length交付。原stdin/权限协议不变；optional mounted配置保留旧路径兼容。

**Tech Stack:** Go1.25.6、原controlprotocol/transport/provisioning、Unix FD/flock、net/http。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-large-download-design.md

**Immutable BASE:** `c8f9a23ca59ff8ea781fb5b709ff968914c9fb31`

## Global Constraints

仅配置file_transfer时启用；缺省保留512KiB下载，上传仍501。原Exec stdin1MiB、wire/authority/fence不变；单Exec、无Lookup/rawfallback/retry、文件bytes不入etcd。max_file默认2GiB且≤2GiB，总磁盘默认4GiB且≤16GiB，active默认2且≤8，timeout默认300且≤HTTPmaxexec；body16KiB、buffer32KiB、stderr64KiB。private目录0700/APIEUID/实际FD/flock/精确stale≤8，无新背景goroutine/Nmap。原终态/drain/exit0+Seal之后HTTP200；取消/Close实际join全部owner；cleanup失败保守收费且可见。Root唯一Go/Docker/Git，apply_patch唯编辑；Sentinel禁止，已关闭OWN/pairs禁止读取/复用。

## Review Focus

1. 目录锁/真实crash/stale：新的进程不能删活跃store文件，也不能泛删未知文件。
2. 配额与IO失败：precharge先于Exec，Close/remove失败不退款/超售，owner未返回不关FD。
3. binary及原大小变化：原FD类型/限额/EOF、0bytes和大于512KiB，不把部分文件当成功。
4. 终态前输出/坏stderr：stdout可读不能先200，signed terminal/appfailure与unknown区分。
5. 慢客户端与关闭：真实写期限/ctx/Content-Length、private磁盘Reader/Writer实际join，错误传播Serve/CLI。

### Task 1: Bounded disk-backed downloads over the original execution

**Files:** 新internal/transferspool/{store,job,files_unix}.go及focusedtests；新internal/sandboxfiles/download.go/download_test.go，必要小幅共享实际regular FD检查；modify cmd/sandbox-launcher/main.go并新download_mode_test.go；deployment/http.go与tests的optional transfer settings/clone；新internal/api/etcdgateway/download.go/download_test.go，files.go窄选择、gateway.go/Close/server.go及原cleanup包装；cmd/sandbox/etcd_serve.go cleanuperror接收；新controlrunner/large_download_native_linux_test.go、cold fixture最小variant和operator文档。确切文件/签名预检冻结，避免大文件塞入已有protocol或gateway主体。

**Consumes:** 原Service.Exec(context.Context,id,requestID,ExecutionRequest,stdout,stderr)(PlainExecResult,error)；terminalExit/executionUnknown/writeServiceError/fileRequest/header验证、mountedHTTPSettings、固定launcher模式及tracked冷fixture。旧taskArchiveStore只作只读模式参考，不暴露能力或当普通文件权限。

**Produces:** transferspool Options/Open/Reserve/Job Write/Seal/Read/Close/Store.Close；sandboxfiles DownloadRequest/ValidateDownload/StreamDownload及strict Failure codec；普通file-download入口；配置后原GETdownload的单Exec有限spool消费；Gateway.Close error传播。具体签名/CLI quiet-error固定在Step1。

- [ ] Step1 只读预检 exact接口、CLItyped退出与stderr schema、spool身份/锁/stale/close、配置/兼容、单Exec/native有限场景；Root裁定冻结并检查producer/consumer。
- [ ] Step2 tests-only spool及普通下载helper/CLI最小RED；Root实际执行与审计后实现，targethost/race/Linuxvet；每个可独立使用连贯切片通过即提交。测试含实际childcrash释放flock，不能用Close替代。
- [ ] Step3 tests-only transfer配置与HTTP接入RED；Root执行后实现可选配置/clone、下载consumer与Close error/CLI生产传播；host/race/Linuxvet及一次受影响旧gateway覆盖后小提交。原终态前无200、预算先于效果、IO/cancel/slowclient/实际join均断言。
- [ ] Step4 新solelarge-download native实际大于1MiB+空/错误有限场景，原16bridge预算内；运行前后真实source/binary/脚本/容器/Wait清理核对；native与operator说明小提交，不重放旧矩阵。
- [ ] Step5 唯一freshreview BASE→HEAD，同pair限定fix；tracked范围验收及剩余上传明确记录，关闭OWN/pair继续总迁移。

## Root producer-consumer conflict scan / self-review

| 接点 | 初检结论 |
| --- | --- |
| transfer settings→Gateway.New | optionalowned配置，loader只读，store仅实际serve打开；旧配置不强制新增hostpath |
| private spool→originalExec writer | 先有限reserve再调用，writer失败cancel，原return之后Seal/读，不能提前关活fd |
| helper→originaldescriptor/receipt | 新普通argv+有限canonicalstdin，wire不变，bytes走stdout；应用Failure只认signedexit1 |
| Seal→HTTP copy | 原执行已终结才HTTP200；长度+实际socket期限约束后续delivery，丢失body不伪完整 |
| Gateway.Close→Serve/CLI | 返回错误影响原cleanup函数类型，逐一适配生产调用与必要测试包装；不另建Close权威 |
| native→cold fixture | 新variant/selector≤16原bridge，原旧行为保持，不扩大场景矩阵 |
| Task内部 | 大文件下载独立可用；上传仍明确缺口，不用stdin扩容或数万chunkExec冒充高效上传 |

Ruling: 完整迁移及持续执行已获授权；有界磁盘暂存是复用已验收原Exec输出、保留成功语义的具体实现选择，不需重开原权限协议/人工审批。若磁盘额度偏保守，可按显式配置调整，不静默改成无界内存或提前200。
