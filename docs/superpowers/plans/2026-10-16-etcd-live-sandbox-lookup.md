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
- [ ] Step2 协议/只读view/service测试RED后实现，host GREEN/race与适当vet，分别提交连贯协议/reader/service切片。
- [ ] Step3 真实TLS etcd/正式launcher/原bridge Lookup成功与健康、到期、并发变化、关闭等拒绝路径验证；Root完整核对实际输入/ELF/原进程及IO joins，小步提交。
- [ ] Step4 唯一fresh reviewer原BASE→finalHEAD审查；同pair修复；tracked验收并关闭OWN，继续HTTP/生产主路径。

Ruling: 本任务是已批准总方案的实际Get/复用前提，不重问用户授权。一次交付完整Lookup，避免只做未消费的健康协议或仅元数据GET；不做新认证模型、HTTP假接线或Redis fallback。
