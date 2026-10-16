# Live Sandbox Lookup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: subagent-driven-development。一个完整查询Task，fresh author/reviewer，连续执行。

**Goal:** 普通service提供按ID的严格当前metadata与实际runtime健康查询。

**Architecture:** 原coherent snapshot/view + view绑定历史runtime读取 + 原pinned bridge上的有限健康exchange，最后重验TTL/原view；沿原service生命周期。

**Tech Stack:** Go1.25.6、现etcd读协议、controltransport/Supervisor/正式launcher/nativefixture；无新依赖。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-live-sandbox-lookup-design.md

## Global Constraints

- immutableBASE2106f825915160b5afc6b51caf6dc8bf3eb9f07d；前OWN/pairs CLOSED，无旧artifact访问或重复任务。
- 原公开WorkspaceIdentity验证与历史链验证不弱化；新view入口只接受同Backend原view，不能由copied records获得权限。
- 请求/响应各≤4096字节、nonce/exact identity/gate绑定，原TLS/EOF/bridge IO join；不使用exec_inspect全/proc采样。
- 单service准入/20秒上限/Close原始取消与join；无N常驻或扫描，不Grant/写状态/创建或exec用户进程。
- 健康前后受控UTC now+1秒严格小于expires，最终Confirm原view；原CreatedAt来自immutable dispatch。
- apply_patch唯一源码/测试/文档/格式编辑；Root唯一精确Git/Docker，Sentinel禁止读写/暂存。固定Go1.25.6。
- 本OWN新epochs，实际GoList17匹配平台/race/前后输入与逐archive成员/scripts/真实ELF；Docker先Root具体preflight再运行并原始Wait/IO join及精确清理。

## Review Focus

- caller伪造或复制view/改IDs：拒绝，不把元数据变native能力。
- health后owner/TTL/gate变化：最终确认或期限检查失败，不返回健康成功。
- 旧响应/错nonce/错误exactUID或boot/gate/坏EOF：有限拒绝并join原连接。
- Close在历史读取或bridge中：单准入取消且等待完成，无晚返回成功。
- TTL续期后CreatedAt/codec默认漂移：仍返回原immutable创建时间、原配置，不按当前expires倒推。

## Task 1: View-bound runtime history, scalar health and actual service Lookup

**Files:** 修改internal/storage/state/etcd/runtime_enrolled_publication_read.go，新增domain_runtime_read.go及对应tests；私有dispatch decoder必要最小调整在runtime_dispatch_read.go，保持公开方法原验证。新增internal/runtime/controltransport/runtime_health_{frame,destination}.go及tests，RuntimeRequest/ReadRuntimeRequest在task_quiesce_types.go/task_close_frame.go增窄typed分支；原Session有限mode/EOF必要最小变更先预检。新增internal/runtime/controltarget/journal_health.go及tests提供无IO的TryLock scalar观察。新增internal/runtime/controlrunner/supervisor_health.go及tests，supervisor_transport.go处理窄分支；新internal/provisioning/plain_cold_lookup.go及tests。实际native只对原cold_create_service_native_linux_test.go增加必要variant，新live_lookup_native_linux_test.go固定selector；不扩大旧全矩阵。

**Consumes:** ReadSandboxSnapshot/原view origin/Confirm、原历史publication/dispatch严格decoder、PlainColdService enter/leave/destination/bridge/pod、受控clock、真实Supervisor.activation/管理TLS/连接所有权。

**Produces:** PlainColdService.Lookup(ctx,id)、同view绑定且caller-owned的publication/dispatch读取结果、有限scalar health exchange。具体DTO/typed错误/locator策略由Step1源代码预检冻结。

- [x] Step1 Root完整接受6c93e1预检：pointer结果/typed notfound与unavailable/v1 nilBusiness+v2owned；原dispatch CreatedAt/private concrete read locator/先后Confirm；health4096含前缀/内部随机nonce/snake_case wire/strictTLS+rawEOF；Journal.ObserveGate TryLock无IO与Supervisor clock前后重验、原service全程准入与最终Close检查。START-GO tests/collectors first，Root审后实际RED，产品随后。
- [x] Step2 RED后实现，协议b3dc75b、reader4a1a71f、Supervisor8ca3712、servicecf6b5e4分步提交；host/race通过，相关Linux vet通过。外部etcd host case明确skip，由真实native重复验证其公开断言。
- [x] Step3 真实TLS etcd/正式launcher/原bridge v1/v2 Lookup与拒绝路径通过；native-local03使用当前source的runner05/launcher05，attach/container原始Wait0，所有进程/IO已join且精确容器已删除。测试提交223c2d2。保留原失败；完整来源与容量限制见验收报告。
- [x] Step4 唯一fresh reviewer原BASE→ab511eb审查发现两项Important；同pair完成实际行为RED、最小修复93f7e4f和targeted/race/Linuxvet/native验证。原reviewer定点复核两项均ADDRESSED，无新增问题；tracked验收见docs/superpowers/reports/2026-10-16-etcd-live-sandbox-lookup-verification.md，关闭本OWN/pair后继续业务操作与生产主路径。

Ruling: 本任务是已批准总方案的实际Get/复用前提，不重问用户授权。一次交付完整Lookup，避免只做未消费的健康协议或仅元数据GET；不做新认证模型、HTTP假接线或Redis fallback。

Ruling: Journal新文件实际命名journal_gate_observation.go，职责仍为原批准的无IO TryLock标量观察。Linux vet要求将既有task_archive_native_test.go反射TypeOf(*s)改为TypeOf(s).Elem()，保留实际反射复制断言。原bridge fixture增可选stdoutObserved，仅观察真实child输出来确定取消发生在实际Cmd.Start之后；不创建产品hook。TaskClose测试辅助函数读取原TLS EOF及原raw adapter EOF后才Close，保持普通bridge严格join。

Ruling: 当前desktop-linux缺少旧测试镜像，native-local03固定使用已有Linux/arm64 kindest/node镜像26ad49adcb6923b3faa00ee6654943de9bdda7cfcb1c42a9b9cef334a48dfb33作为外层文件系统；只运行当前正式launcher与etcd绑定，不运行kind服务。此Task不验收生产镜像或容量。GoList的17字段保守清单包含product metadata中的TestGoFiles，故测试修改后另做launcher05来源捕获；其VCS构建元数据与04不同，最终用05双binary实际重跑一次。
