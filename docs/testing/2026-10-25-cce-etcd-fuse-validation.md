# CCE etcd + FUSE 独立安装验证（2026-10-25）

## 范围与安装

目标为 `internal` context 的 arm64 CCE 集群、`sandbox-fuse-etcd` 测试 namespace；未修改原有 `aiadp-sandbox-fuse` release。源码为 `feat/etcd-state-management` 的 `4324db0`，Chart 为同一提交的 `deploy/helm/sandbox`。测试 API 镜像是 `sandbox-fuse-etcd-api:v1-capacity-68f98b9-arm64`；此后至 `4324db0` 的 API Go 源码没有变化。mounter 从 `4324db0` 构建 arm64 ELF，镜像固定为 `sandbox-fuse-etcd-mounter@sha256:5395497568187f4b6fb617c3ee3a7a3352b06fb78dd4dad0e7c8b9c4154970cc`。runtime 使用 DS 已验 digest `sha256:ec108aeee183c1cb0f3a5c481bf38038626fde8da96ae5a4624df396d5f6d7d8`。

测试依赖是本 namespace 的三成员 etcd 和独立 MinIO；三名 etcd 成员分别位于 `.35`、`.29`、`.66` 三节点。它们使用 **emptyDir**，只用于功能验证；Pod 重建会丢失该成员本地数据，不能把本轮当作持久卷、备份或 etcd 故障恢复验收。`csi-local` 1 GiB PVC 探针持续 Pending，已删除。测试 Secret 仅存于 namespace，未写入仓库、报告或 Helm values；MinIO bucket 为 `cce-test-bucket`。

部署配置为 etcd 明文三 endpoint、独立 `/sandbox/cce-test-20261025/` 前缀、Redis 关闭、FUSE 默认、普通与 FUSE 准备池各 `min=3/max=20`、AppArmor loader 启用。`max=20` 仅限制准备池，不限制存活沙盒数。API 两副本、loader 三副本、etcd 三成员和 MinIO 单副本均 Ready；Helm release `sandbox` revision 2 为 `deployed`。未创建对外 Ingress，也未切换其它业务流量。

## 现场结果

| 检查 | 结果 |
| --- | --- |
| CRI AppArmor 预检 | 在 `.29`、`.66` 节点启动带现有 Localhost profile 的 mounter 容器，均有 containerd ID 和 `Started` 事件；`--help` 退出码 1 是测试命令结果，不是 CRI 拒绝。两个探针 Pod 已删除。`.35` 节点另有原 release 的 FUSE mounter 运行。 |
| 首次 Helm 安装 | **失败**。CCE NodeLocal DNS 准入改写 loader Pod 的 DNSPolicy、nameservers、searches；严格模板校验找不到匹配的 Ready Pod。API 在 180 秒门禁后报 `AppArmor loader startup gate incomplete`。不能把该次失败改判成功。 |
| 修正后安装 | 按仓库既有 DNS 兼容合同设置 `disableNodeLocalDNSInjection=true` 与 `single-request-reopen`、`timeout=2` 两项，Helm lint/template 通过；revision 2 安装成功，API `GET /ready` 返回 200。没有放宽 AppArmor 或 Pod 身份比较。 |
| 公开 FUSE 主链 | 首个沙盒 POST 201，约 1.43 秒；`/workspace/index.html` 内执行读写返回 200；`from_container` sync 返回 200，约 0.44 秒；文件下载返回 `text/html; charset=utf-8`，MinIO 对象 `Content-Type: text/html`、大小 38 字节。 |
| 跨副本与删除 | 另一 API Pod GET 同一沙盒返回 200/ready；由该 Pod DELETE 返回 200，约 2.01 秒；两个 API 入口随后均返回 404，原运行时 Pod 消失。 |
| 自然 TTL | 45 秒 FUSE 沙盒到期后公开 GET 返回 404，原运行时 Pod 消失。 |
| API 重启 | 第三个存活 FUSE 沙盒先写入 `/workspace/restart.txt`；API Deployment 滚动重启完成后，新 API Pod GET 仍为 200/ready，Exec 读回原内容，公开 DELETE 返回 200，运行时 Pod 消失。 |
| 跨节点实际挂载 | 另建三个 FUSE 沙盒，运行时依次落在 `.35`、`.29`、`.35`。`.29` 的 `sandbox-pool-wl69339j4x` 通过 Python Exec 在 `/workspace/cce-node-check.txt` 写入并读回 `cce-cross-node-ok`，HTTP 200、退出码 0；三个公开 DELETE 均返回 200，原三个运行时 Pod 随后均不存在。 |

六个测试沙盒均已公开释放，并确认对应运行时 Pod 消失。沙盒 DELETE 不删除工作区对象；跨节点补测后，MinIO 仍有三个本轮测试前缀及其中一个 17 字节文件。随后只删除这三个测试专用前缀，再次递归列举 `workspaces/` 为 0 个对象；首轮三个测试前缀也已单独清理。端口转发与 AppArmor/PVC 探针均已结束。临时 etcd、MinIO、API、Secret 和准备池仍保留在专用 namespace，供后续 CCE 测试；清理时只处理本轮拥有的资源，不能触碰 `aiadp-sandbox-fuse`。

## 尚未验收

- emptyDir 依赖不能证明 API/etcd/MinIO 的持久卷恢复、etcd 成员重建或备份恢复。下一轮须使用明确持久化方案后再测。
- `.29` 与 `.35` 已有实际 FUSE 创建和 `/workspace` 读写正例；`.66` 仍只有 CRI profile 启动预检。跨节点正例没有逐节点覆盖 flush、卸载故障或 AppArmor 越权拒绝，不能外推为全节点验收。
- 未测大量存活沙盒、集中 TTL、持续吞吐、CNI 隔离执行或真实业务流量。此前 DS 的 20 个驻留正例与本轮 CCE 单沙盒正例均不能外推为 10 万容量。
- 性能代码审查仍见 `guardFUSEPoolRecord` 对每次候选检查逐个列出/读取 session、ephemeral 与 workspace owner；按存活 N 增长的远端读取会影响补池和申请。需要保持原子发布与保守清理语义，再改为可验证的定点索引；本轮没有宣称性能提升。周期活跃扫描与到期调度也需独立大存量验收。

独立安装步骤见 [etcd + FUSE 全新部署手册](../deployment/etcd-fuse-fresh-install.md)。

## 2026-10-26 后续状态

上文记录的是 revision 2 当时的安装结果，不代表当前集群版本。CCE
`sandbox` 已升级到 revision 12，API 2/2 Ready，镜像为
`sandbox-fuse-etcd-api:v1-publication-guard-85b2b98-arm64`。后续测试
覆盖了真实 FUSE 文件读写、公开创建与删除、API 重启、创建中断后的
占用恢复，以及执行、技能、分片文件与两个私网目标的网络观察；逐次
证据与限制见 [发布与容量报告](../superpowers/reports/2026-10-24-etcd-capacity-and-release.md)。

CCE 的三个测试 etcd Pod 当前仍使用 `emptyDir`，namespace 内没有
etcd PVC。它们可用于上述功能测试，不能作为持久卷重建或全员数据
丢失后的恢复验收。自己部署时应按部署手册提供独立持久卷与备份方案；
大存量容量仍单独验收。
