# Etcd File API Implementation Plan

> For agentic workers: REQUIRED SUB-SKILL subagent-driven-development；一个完整文件API接入Task，唯一fresh author/reviewer，小步验证提交。

**Goal:** 显式etcd服务提供八条真实有界文件路由，独立有限health/ready准入。

**Architecture:** pinned launcher的无特权file-operation由原service.Exec执行，参数和内容走有界stdin/stdout；复用原authority/Wait，文件内容不入etcd。原HTTP入口与CLI不新增第二套runtime。

**Tech Stack:** Go1.25.6、os.Root、net/http、现有controlprotocol/provisioning。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-file-api-design.md

**Immutable BASE:** `506800a4a680328abd8a1a7b540546162d9aea1c`

## Global Constraints

仅显式etcd模式；原signed terminal/drain/Wait才成功，unknown不重发。无Redis/raw Pod exec/fallback/额外Lookup；固定非特权UID/GID、baseenv/workdir/timeout。helper stdin1MiB、内容512KiB、stdout1MiB/stderr64KiB、目录10000条/深度64、页1000、path/pattern4096。APIbody2MiB、business≤32、health4/ready2独立lane，Close取消且实际join全部lane。单Exec、文件内容不写etcd，无按resident增长map/worker。Root唯一Go/Docker/Git，apply_patch唯编辑；Sentinel禁止、已关闭OWN/pair禁止访问/复用。

## Review Focus

1. symlink与FIFO实际fd行为：路径表面合法也不能逃逸/无限阻塞；真实临时目录测试。
2. binary/换行名/无末尾换行/空白行：不能通过shell/tab协议损坏或误计；精确bytes测试。
3. 编辑失败或lostACK：原目标不预截断，不因错误假称没改/自动重试；原unknown refs测试。
4. valid-lookingstdout但原receipt不成功：不能返回成功，输出溢出取消并join；gateway wire测试。
5. 业务和ready占满、Close竞态：health独立成功、closed统一拒绝、所有owner join；真实socket测试。

### Task 1: File helper, eight etcd HTTP consumers and independent probes

**Files:** create internal/sandboxfiles/{protocol,files,serve}.go与针对tests；modify cmd/sandbox-launcher/main.go并新增file_operation_test.go；create internal/api/etcdgateway/files.go/files_test.go、probe_test.go并modify gateway.go；必要修改json/exec的小幅共享，不复制终态/错误分类；新internal/runtime/controlrunner/file_api_native_linux_test.go与cold_create_service_native_linux_test.go的最小独立variant；新增operator文档。最终确切签名/源点在Step1预检冻结，无超范围架构变动。

**Consumes:** `PlainColdService.Exec(context.Context,string,string,controlprotocol.ExecutionRequest,io.Writer,io.Writer)(PlainExecResult,error)`、现有terminalExit/executionUnknown/writeServiceError、strict readRequest、mounted HTTPSettings、原native cold fixture。

**Produces:** `sandbox-launcher file-operation`普通child、有限Request/Result/Validate/Run/Serve、八条原DTO路由；单一gateway三lane生命周期与新native证据。

- [x] Step1 只读预检已冻结 exact Request/Result/Validate/Run/Serve/DecodeResult/ErrorStatus、原DTO、单Exec、三lane与native14bridge；实际parentFD/rawOpenat，不新增生产hostroot参数。
- [x] Step2 helper/CLI实际RED→实现→host/race及Linuxvet，提交2f0dc3b。Helper各9PASS/1明确Darwin非法文件名SKIP，Linux该例待新native；launcher各2PASS无skip。原EILSEQ失败和Root一次selector漏wrapper记录保留，不把它们称为完整通过。
- [x] Step3 新HTTP/probe实际RED→实现，全部23项gateway host/race PASS无skip、Linuxvet通过。八route descriptor/单Exec/receipt/strictbody/unknown/overflowjoin和独立health/ready满载Close通过；共享路由/生命周期变化已覆盖原gateway，无旧native矩阵重跑。
- [x] Step4 新TestFileAPINative实际PASS2.06s无skip，真实TLS-etcd/正式launcher/UIDGID1000、Linux非法文件名422及八路由/二进制/行/编辑/下载通过。14个原bridge Wait/IO、正式launcher Wait0、etcd SIGTERM已join；attach/container0、无OOM、精确清理。原警告/teardown日志保留；未重放旧矩阵，不计大N或生产验收。
- [x] Step5 唯一fresh reviewer BASE506800a→HEAD9ba1fbb，spec compliant/质量Approved，C0/I0/M1，无待补检查；仅原fixture日志噪声延期最终整分支整理。tracked范围验收/整体状态更新，关闭OWN/pair，继续大文件及剩余迁移。

## Root self-review / producer-consumer scan

| 接点 | 检查结论 |
| --- | --- |
| helper Request/Result→gateway | 同包共享envelope，不手写两份；stdin/json/base64和stdout预算叠加预检查 |
| helper→launcher→原Exec | 固定现有pinned路径，新增普通子命令，无新的控制凭证/管理listener；原descriptor允许绝对argv |
| actualfilesystem→API | os.Root处理实际路径；非特权FS结果需原terminal再消费，不将helper应用结果当runtime证明 |
| gateway file routes→原DTO | 保留rawread/download和精确字段；有限容量与lines差异显式文档 |
| probes→原Close | 三lane总owner≤38，closed锁登记与WG Add不分离；ready服务容量仍真实反映不可用 |
| native→fixture/launcher | 只新增file variant和selector，原HTTP/Exec已验收任务不重放 |
| Task1内部 | 八路由/helper/探针均在此可独立运行单元，上传仍501是已明确的下一单元边界 |

Ruling: 用户已批准完整迁移并要求持续加速；本单元是原业务接线的具体可逆实现选择，继续既有委派/小步提交流程，不重新申请授权。若helper与旧镜像不匹配，显式模式需更新pinned launcher，operator须写明，不静默回退rawexec。
