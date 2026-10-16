# etcd Startup Loader Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: subagent-driven-development；一个完整Task，连续推进，不在Task边界停下。

**Goal:** 为现有etcd创建服务提供实际挂载配置装配和只读命令行部署检查。

**Architecture:** 新deployment包严格读取manifest与凭证，复用原Backend/签名时钟/PlainColdService；cmd/sandbox在旧配置及Redis之前显式分流检查模式。

**Tech Stack:** Go1.25.6、现etcd v3/client-go/Ed25519协议；不新增依赖。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-startup-loader-design.md

## Global Constraints

- immutableBASE1161ee4e11150dab03ffa29cc55b3ac80eeed034；前任务OWN/pair CLOSED，无旧artifact访问、无重复实现。
- apply_patch唯一源码/文档/格式编辑方式；Root唯一Git/实际Docker，精确小提交，禁止Sentinel读写或暂存。
- manifest64KiB、TLS各64KiB、launcher16384B、issuer8192B、私钥4096B；所有实际读有界，拒绝特殊文件，支持投影Secret链接。
- 生产HTTPS/mTLS、固定身份/受控签名时钟，禁止本地时间fallback及构造器自动初始化库存/写状态/Grant/POST。
- Close先Service后Backend，部分失败回收；无每N常驻任务、无新全量扫描。
- 单author+唯一fresh wholeTask reviewer，无nested。测试新epochs、实际GoList17类输入匹配平台/race、前后归档及脚本；Docker另需Root具体命令preflight/原始Wait和精确cleanup。

## Review Focus

- 缺失/超大/特殊文件或重复JSON字段：有限失败，不泄漏原始凭证。
- kubeconfig含外部exec/auth插件：明确拒绝，不能在只读检查中执行操作员未授权程序。
- 旧特殊模式混用/缺参：在任何Redis或runtime动作前拒绝。
- inventory缺失、身份或证书/时钟错误：沿原构造验证失败，已开资源关闭/join。
- 构造成功：实际Backend+Service存在、无Pod创建/Lease/状态变化，重复Close安全。

## Task 1: Mounted deployment loader and executable startup check

**Files:** 新internal/provisioning/deployment/{manifest.go,files.go,open.go}与对应tests（职责小幅调整先报告）；新cmd/sandbox/etcd_startup.go及etcd_startup_test.go，main.go仅flag/互斥/早分流。原internal/runtime/controlrunner/cold_create_service_native_linux_test.go可最小增加真实装配验证variant，新增startup_loader_native_linux_test.go。部署样例/说明放docs中本Task命名文件，不读取其他Task OWN。

**Consumes:** state.New/Namespace/Identity/PreparationInventoryOptions/PublicationTrust/EnrolledPublicationOptions、p.NewSignedClockClient、launcherconfig.Decode、client-go配置读取、provisioning.NewPlainColdService与其Close。

**Produces:** deployment.Open(ctx,path) (*Deployment,error)、Deployment.Close() error及Service/Backend受控访问；cmd启动检查早分流。具体DTO/导出访问方式由Step1只读预检冻结。

- [x] Step1 Root接受完整预检9f583b：DTO与单一launcher信任来源、bounded文件/内联kubeconfig、原构造顺序、Close所有权、CLI早分流与真实TLS+clock无写入场景已冻结到spec。检查总预算30秒，etcd TLS最低1.3，config与所有显式旧modeflag拒绝。START-GO tests/collectors first，具体Go仍经Root审collector；native无需启动launcher。
- [x] Step2 实际missingAPI编译RED后实现；修复Unicode JSON别名与child test参数分隔。最终host/race九组PASS，无skip/race报告；Root核对11061/11066实际输入与全部归档。377e627提交loader，3f447df提交命令及说明。
- [x] Step3 Linux vet及runner/CLI真实ELF构建均退出0；Root核对9141/10769/11284实际输入、全部成员与实际Go1.25.6 buildinfo。native1夹具/run权限失败已保留并清理；仅修改临时目录权限后native2真实TLS etcd/签名clock九项检查通过，四次CLI Wait符合预期，库重复Close成功，KV/revision/Lease不变、所有Kubernetes请求0，clock/etcd进程join、容器退出0无OOM且精确清理。3a6dfb0提交真实集成测试；无容量或HTTP切换声明。
- [ ] Step4 唯一fresh reviewer审原BASE→最终HEAD；必要同pair修复；tracked验收、关闭OWN，继续实际HTTP与生产互斥接线。

Ruling: 用户已批准总迁移并明确持续执行；这是已批准生产接线的具体化，沿既有执行方法，不重复索要授权。采用显式检查入口，完整服务切换留在具备所有endpoint能力之后，避免nil旧Manager或Redis fallback。
