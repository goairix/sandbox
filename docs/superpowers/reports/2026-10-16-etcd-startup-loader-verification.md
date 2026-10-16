# etcd 启动装配验收

2026-10-16。独立审查范围`1161ee4e11150dab03ffa29cc55b3ac80eeed034`→`158c93de4734efb53e2bd33d26ff06cf5c4c429b`，六笔提交、13个文件。结论：Spec compliant / Approved，无Critical或Important问题。

## 交付

- `377e627`：严格manifest、有界挂载凭证读取、静态kubeconfig内联、原Backend/签名时钟/创建服务装配和关闭所有权。
- `3f447df`：实际`--check-etcd-startup`命令在旧配置与Redis之前分流，30秒总预算，关闭完成后退出；附配置说明。
- `3a6dfb0`：真实TLS etcd、签名时钟及sandbox可执行文件集成测试。

## 验证

Go1.25.6下最终host/race九组测试全部通过，无skip或race报告；相关Linuxarm64 vet退出0。host最终轮loader包使用此前通过的测试缓存，主程序测试重新执行。真实ELF和实际buildinfo确认runner及sandbox均为Linuxarm64/CGO0。Root核对实际17类GoList输入、前后清单、归档全部常规成员与脚本；输入数量分别为host11061、race11066、runner9141、CLI10769、vet11284。

native2九项检查通过：库存未就绪、成功Open/重复Close、身份不匹配、错误时钟密钥和错误issuer；其中四次实际命令进程退出符合预期（一成功、三拒绝）。成功路径使用只读etcd角色。每项前后完整KV/revision、Lease清单不变，所有Kubernetes请求为0。签名时钟Serve与原TLS etcd进程均join，容器退出0、无OOM，精确删除并确认本次容器清空。容器保持非root、无额外capability、网络隔离及只读根目录。

保留真实失败及修复：缺失API的编译RED；子进程测试缺少参数分隔符；Bash空数组导致一次CLI采集在Go执行前失败；首轮native因临时/run权限失败。最终native只调整私有tmpfs权限，未修改产品源码或重建已验证二进制。原etcd配置和关闭诊断保留，为审查Minor项，不宣称日志无噪声。

## 剩余范围

本命令验证既有部署配置及构造器，不初始化身份/库存，不申请Lease或创建Pod。普通HTTP主进程仍使用Redis；实时健康与完整业务接口、分区调度、FUSE/collector、容量、部署恢复及Redis移除尚未整体完成。此次不构成性能或容量验收。
