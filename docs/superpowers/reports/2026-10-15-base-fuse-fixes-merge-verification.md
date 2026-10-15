# 基分支 FUSE 修复合并验证

用户2026-10-15明确要求先合并feat/workspace-fuse-mount，再继续etcd迁移。成功fetch其hxsy upstream，local/upstream/FETCH_HEAD均为c14a52ccfe9d1d215620e2be85abf521a6556355。

合并提交45ebc70398e492f23b7962a2223bc85c3950e70f，父提交980f0e6和c14a52c，ort无冲突。合入629540d（恢复记录）、f96b7b5（FUSE终态回包丢失后重新核验）、0b7cac6（cleanup恢复/升级缺口）、c14a52c（Kubernetes服务环境变量清理），共45files。没有触碰原有Sentinel文档工作区改动，没有推送或部署。

## 合并后验证

- Go1.25.6 Darwin/arm64，cmd/sandbox、internal/fuseprotocol、internal/mounter、internal/runtime/kubernetes、internal/sandbox、internal/workspaceprobe各包race PASS。输出含1687个test PASS事件（包含父测试）和23个SKIP事件，不将事件数当unique叶子数或全平台验收。
- 首次组合命令同时列入internal/helmtest但遗漏helmtests标签，故整体exit1；上述六包仍各自PASS，无测试断言FAIL。保留原输出，不为得到全绿重跑已通过包。
- internal/helmtest以-tags=helmtests限定TestCleanup/TestDNSAdmission重新运行：包PASS/exit0，45个PASS事件。只执行真实Helm模板渲染，不操作集群。
- 对六个普通包go vet exit0；最初误包含无标签Helm包的vet setup错误同样保留。
- GOOS=linux GOARCH=arm64 CGO_ENABLED=0构建cmd/sandbox exit0。它是编译验证，不代表Linux专有测试已重新执行。

原始命令、JSON/stderr和exit结果位于当时OPEN OWN的base-merge-2026-10-15子目录；未重跑旧etcd验收单元、未声称生产切换或容量验收。

## 对迁移的新增约束

基分支新增LoadWorkspaceControlKey，通过原state.Store.SetNX/Get永久保存每scope的probe签名seed；main当前接的是Redis。它是跨API/重启的fencing历史，不能在Redis移除时丢失、遇读取异常时另造新key，或复制私钥进Pod。后续生产接线必须明确持久etcd authority或等价的新原生控制链及旧版本排空，不可仅删Redis调用。旧生产缺口图中“未发现credential必须依赖Redis”的判断早于这次基分支变更，须以本补充为准。
