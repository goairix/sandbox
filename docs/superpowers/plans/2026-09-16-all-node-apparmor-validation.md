# 五节点 AppArmor 公共 API 验收实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: 使用 executing-plans 内联执行。用户已确认隔离命名空间设计；不使用子代理、worktree、业务 release 升级或节点重启。

**Goal:** 在 worker-1 至 worker-5 各完成普通/FUSE 真实 API、s3fs exact enforce、拒绝负例与自己 loader 的 Pod 重建恢复。

**Architecture:** 一次仅一个节点实验，来自同一 Chart 的 API/loader/runtime 通过原 nodeSelector 配置固定调度。独立 standalone Redis 无 PVC，沿用当前可信 TLS 存储，仅随机实验前缀。所有资源带 run 归属；清理用正常 sandbox DELETE、Chart drain Job、UID 前置条件 namespace DELETE 与 ClusterRole/Binding 回收。

**Tech Stack:** Node.js 标准库、系统 Ruby YAML 解析、kubectl、Helm、本集群已部署镜像 digest。

---

## Task 1：安全驱动 RED / GREEN

Files: 新增 `testdata/apparmor-enforcement/all-node-api.js`、`all-node-api.test.js`。

- [x] 在无网络 stub 中测试 namespace 归属拒绝：错误 UID、错误 run/purpose、业务 namespace 均不得写；Pod replacement UID、nodeName 不符不得 exec/delete。先运行 `node testdata/apparmor-enforcement/all-node-api.test.js` 观察 RED。
- [x] 实现只接受 context `ds-ai-research`、节点 `ds-ai-worker-[1-5]`、随机 hex run 和 `sandbox-aa-api-<run>-<ordinal>` 的 guard。调用 mutation 前 GET namespace 并检查记录 UID 与标签；精确 Pod 再检查 UID、运行节点、无重启。

```js
function assertNamespaceIdentity(object, expected) {
  if (object.metadata.name !== expected.name || object.metadata.uid !== expected.uid ||
      object.metadata.labels['sandbox-test-run'] !== expected.run ||
      object.metadata.labels['sandbox-test-purpose'] !== 'apparmor-all-node-api') {
    throw new Error('lab namespace identity mismatch');
  }
}
```

- [x] dry-run mode 不读业务 Secret、不创建任何集群资源。真实 mode 要求 `--execute --context ds-ai-research --node ds-ai-worker-N`；不使用默认 context 或接受任意 namespace 参数。
- [x] 重跑无网络 guard、语法检查 GREEN；每次运行的 journal/report 只存非敏感身份与阶段，临时目录 0700、配置 0600。

## Task 2：Chart 资源与身份固定

Files: 继续 `all-node-api.js`；不修改 Chart/profile/生产 values。

- [x] 只读核对业务 Deployment/StatefulSet/DaemonSet UID/template/images/Ready、五节点条件；核对 Chart profile hash 为 `37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d`。
- [x] 从业务 API 私有 env/Secret 引用在内存解析当前 MinIO AK/SK（不返回输出），要求 TLS=true、bucket/endpoint 可用。随机子路径 `validation/apparmor-all-nodes/<run>/<node>/`，每个 workspace 再用自己的相对 path。
- [x] 创建唯一 Namespace（run/purpose 标签），记录 API 返回 UID；创建自己的 API-key Secret，随机 API key 只在内存。使用 Chart values：replicaCount=1、autoscaling/PDB=false、Redis standalone/persistence=false、普通/FUSE min1/max2、缺失LSM豁免=false、loader=true，nodeSelector 固定节点；telemetry=false。
- [x] `helm template` 只选择 serviceaccount/rbac/runtime-role/runtime-rolebinding/runtime-network-inventory/secret/service/redis/deployment/apparmor-loader/networkpolicy。stdout 只在驱动内存；Ruby YAML 转 JSON，不将凭据输出。render 资源逐项限定 namespace/kind，cluster inventory 两对象只读规则且 Namespace ownerReference exact UID。
- [x] image 用已核验 digest：API `1e8e95347f805417998d4555d6d107d37e0895929bed8d095b33eda79f15cd4a`、runtime `237bd749531b31eb6144b5ce2d4b6dc93232c39bae2989c6bb9a4797a575e8c8`、mounter `56db3a0e82c36259ff5325cf5294a5c3b876c085b8ccde147c41c76bca2e8931`、loader arm64 `5a984dcb978f6634acee4446bb2f1ae5d2aca918ee53b189372deb688bcf908c`。Redis 使用当前已部署镜像；不读取/修改 business Redis 数据。
- [x] 创建后每 3 秒读取自己的 Ready，启动总预算 240 秒；单次 kubectl 30 秒。准备失败输出白名单状态，不打印 API env/Job 原始日志。

## Task 3：每节点真实生命周期

- [ ] 只转发自己 API 的精确 Pod，本地随机端口绑定 127.0.0.1；每请求总预算 45 秒，响应上限 2 MiB；只输出 status/阶段，不输出凭据或错误 body。
- [ ] 读取自己的 pristine 普通/FUSE pool Pod UID/nodeName/零重启，API 返回 runtime_id 必须匹配 pristine 身份。
- [ ] `POST /api/v1/sandboxes {mode:ephemeral,timeout:300}`；`POST .../:id/exec` 执行 Python/Node/bash 固定 hello、写读 fsync、读取 uid=1000/NoNewPrivs=1/Seccomp=2/默认 enforce。`DELETE` 后精确原 UID 不存在、对应策略无残留，prepared Pool 恢复。
- [ ] `POST /api/v1/sandboxes {mode:persistent,timeout:300,workspace_path:api-workspace,workspace_mount_mode:fuse}`，用真实 sandbox Python 写/fsync/读回、覆盖、追加、rename、unlink。`POST .../:id/workspace/sync {direction:from_container}` 后 `GET workspace/info` 要求 mounted/mount_type=fuse/flushed。
- [ ] 独立 SigV4+验证 TLS 的对象 GET 必须匹配 final 文件内容。读取 `/proc/1/attr/current`、真实 s3fs PID/ppid/startTime/profile 与 mountinfo mount ID。用只读观测组合 exact UID/generation1/零重启，不用 shell profile 冒充 s3fs。

## Task 4：拒绝审计与允许对照

- [ ] 仅自己的 loader 做 unconfined 允许对照：shadow 可读但读取重定向 /dev/null；/dev/shm 写删除；/run/apparmor-loader/<run> 内 tmpfs mount/umount/rmdir，所有命令必须返回 0。
- [ ] 仅自己的 mounter：`cat /etc/hosts >/dev/null`、写 `/run/s3fs/<run>-allowed` 成功；读取 shadow、写自己 /dev/shm 路径、已受限 shell `exec /bin/sh -c true`、初始 `/bin/mount -t tmpfs ...` mount 自己 cache 子目录必须拒绝。stderr/stdout 只在内存，非预期成功固定失败消息，不打印 shadow 内容。
- [ ] 用同一 exec PID 的唯一 /dev/shm 拒绝 marker 与 `exec` 子命令关联 kernel PID；mount 用唯一 cache 目录关联。自己的 loader 读取 dmesg 后在内存筛选 exact profile+唯一 marker/PID+测试新时间窗，只报告 DENIED 规则/计数。
- [ ] 要求各负例有对应 operation=open/mknod(or open create)/exec/mount 的 kernel DENIED；utility 缺失、只读 FS、测试身份错或控制读失败不能算 AppArmor 通过。
- [ ] 重新核验真实 s3fs、API 固定读回和 flush 仍正常；删除实验业务文件再 flush，对象 GET 404；正常 DELETE 确认原 Pod 与策略收尾，Pool 恢复。
- [ ] 仅用 UID 前置条件 DELETE 自己 loader Pod，等待同节点新 UID Ready/零重启，exact profile 仍 enforce。禁止策略卸载、业务 loader 删除或节点 reboot。

## Task 5：精确清理与证据

- [ ] render/create 同 Chart `pre-delete-drain.yaml` 为自己 Job，先缩自己 API 至零、release-wide drain，require Job Complete、managed sandbox Pods/policies=0；不使用 Helm uninstall 业务 release。
- [ ] UID 前置条件 `kubectl delete --raw /api/v1/namespaces/<ns> -f -`，DeleteOptions 内 exact Namespace UID，不 force 或移除 finalizer。inventory ClusterRole/Binding ownerReference 与 UID 逐项验证收尾。
- [ ] 停止自己的端口转发、移除自己精确临时凭据文件；保留非敏感 report。失败时先安全清理正常 API sandbox/drain；若清理未确认保留 namespace UID 和阶段并停止下一节点，不裸删状态。
- [ ] 每节点完成后核对业务三对象 UID/template/images/Ready 不变；顺序执行 1–5，各自 report 包含运行版本/身份/阶段 PASS。最终全局检查 test namespace/RBAC 无残留。
- [ ] 新增 `docs/testing/2026-09-16-all-node-apparmor-api-results.md`，逐节点成功/失败/未执行，不扩展为节点冷启动、Redis HA 或其它 CNI 验收；更新最新后续清单 SEC-01 覆盖边界。
- [x] 无网络驱动安全测试、`bash scripts/test-helm-apparmor-loader.sh`、diff 检查、自审后提交驱动和当前阻塞记录（不代表五节点现场完成）。用户待提供环境再测第 3 项，FUSE 性能继续暂缓。

## 2026-09-16 18:19 进度 / 阻塞

驱动、无网络安全回归和 Chart实验搭建已完成；worker-1/2/3完整生命周期、四类内核拒绝、
自己loader Pod重建、drain和精确清理全部通过。审计夹具的uptime时间域偏移与kauditd printk
限流已修正为同时间域水位和有界缺失负例重试，不改内核参数或策略。

worker-4首次API镜像拉取期间超时，但清理确认。第二轮API启动后，普通runtime镜像拉取/终止
链路未完成；API重启、启动超时，正常drain失败，原ordinary Pod删除未确认。按设计保留
`sandbox-aa-api-17eaec31-4` / UID `d3010735-8934-44df-8e42-269750fb0589`，停止worker-5。
没有force、手工去finalizer、裸删状态或业务/节点重启。自己的私有本地values已移除，report保留。

任务3–5的五节点汇总仍未完成，不能将本计划标为完成。现有Node日志查询未启用，SSH主机名
不可解析，需要节点访问入口或脱敏kubelet/containerd日志；先定位节点、正常收尾保留run，再测4/5。
详细身份/证据见 [验收报告](../../testing/2026-09-16-all-node-apparmor-api-results.md)。
