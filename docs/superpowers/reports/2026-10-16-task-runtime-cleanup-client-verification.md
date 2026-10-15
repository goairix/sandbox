# Kubernetes 精确清理客户端范围验收

本次验收具体 Kubernetes client，供正在实现的 etcd 终止与安全释放链路消费。它没有接 main，没有完成物理终止、owner 释放、真实 kubelet/部署或大 N 容量验收。

## 源码与审查

Task BASE：`15e4eec600adea133e1b79646bca51ba7638d727`。三个连贯实现提交：

- `1cb993cc11b4931d24654a7d4293a53f18924b43`：具体配置、严格 descriptor、精确点读、有界 owner 与实际 Close 等待。
- `2689e5de9e1f8a6926422fdc490fc26b84eb4586`：固定七动作，ObserveStep 保存 UID/RV，Apply 消费同一 snapshot，漂移拒绝、未知不重发。
- `402fbc03594866f12661aa9bfb4b30c809309d0c`：原 body/dial join、硬响应上界、真实 authority 输入、禁止隐式重发。

唯一 fresh 完整审查指出一项 Important：live policy UID 和 saved descriptor target UID 没有验证规范非零 UUID。修复提交 `e2aa59e0a891e62086a1ccc78991b3185400f6ad` 共两个路径、113 insertions/2 deletions；同一 reviewer 限定 R1 确认 I1 ADDRESSED，无新增 Critical/Important/Minor。无开放产品 finding。完整审查与 R1 都没有重跑测试矩阵或派生其他 reviewer。

完整 Task 过滤包仅含三条本 Task 实现提交/八路径，72,783 bytes，SHA256 `c21d63fbbd63adb5a5228f2b622ec2fb1eb325c747dc543025f72e75c0e3e6ef`；其余交错的 etcd/文档提交未混入客户端审查。R1 限定包两个路径、10,635 bytes，SHA256 `512be2eed2f1056922fbbb069682f02c22d344a2c5e92ab4846d76e10bce4a40`。

## 行为与边界

客户端只有固定 Pod、Node、logical/runtime 名称对应且去重的最多四个 policy 的 point GET；不 List，不调用旧 bundled cleanup，不删除替换 UID、shared operator 资源或其他 finalizer。仅支持本方案 ordinary enrolled plain profile，允许零挂载或精确默认 workspace/tmp EmptyDir，拒绝外部/未知挂载合同。

七槽为安装自己的 finalizer、原 Pod DELETE、四个 policy DELETE、移除自己的 finalizer。ObserveStep 的 UID/RV 必须先由 Backend 写持久 intent 并执行 native guard，再 Apply；客户端不授予这些权限。Apply 消费原 snapshot，不自动升 RV、不因只读查询重置 consumed/unknown。Install 的 Retained、Pod 的 Deleting、各对象 Absent/Terminal 都只是具体 API 诊断，不能证明物理死亡或全链路完成。

MaxActive 1..32、请求期限正值且不超过 5 秒，descriptor 不超过 16 KiB，请求及每个 response/body 不超过 256 KiB。原 body/dial 实际返回后才退款，Close 封闭新准入并等待原工作。发布后的 TLS/config 容器不改写，调用者原配置保持。专用 HTTP/1、禁连接复用/HTTP2 replay/redirect、显式 MaxRetries(0)。这些是组件界限，不是 cell 全局容量或线上性能结果。

## 实际验证

实现阶段最终有限 race selector `^TestTaskRuntimeCleanupClient`：12 top-level、55 total test events PASS。真实 TLS server 收到正常七动作共 7 次 mutation；logical/runtime 同名去重后 5 次；9 类身份/版本/资源漂移均零 mutation。429+Retry-After、307、308、broken connection 各跨七个动作，共 28 个有限 case，各实际收到一条 mutation，后续诊断没有恢复重发权限。

原行为 RED 实际暴露两项错误：超界 chunked 404 被 SDK 转为成功 absence，parent 取消后残余 dial 尚未返回便提前退款。修复后两个硬响应上界 case、原 dial/body/Close join 与额度保留、调用者 TLS 容器不变通过。真实 namespace 512/513、原非 UUID restore epoch 与 opaque authority ID/target 的消费者输入缺口也通过新增 RED→GREEN 修复；没有改变旧协议 validator。

R1 仅运行新 `^TestTaskRuntimeCleanupClientPolicyUID$`：1 top-level、17 total events PASS。NetworkPolicy/CiliumPolicy 的 live/saved malformed、nil UUID、非规范大小写共 12 负例零 mutation；saved 负例搭配 live absent，避免靠 UID mismatch 假通过。两个合法 UUID 各一条 DELETE，两个 absent 路径零 mutation。Root 独立同 selector race、受影响 vet、格式与 diff 检查通过；没有为 R1 重跑旧 55/28 矩阵。新代码的验收依据为原完整结果加本次限定增量，不将旧轮描述成最终源码的整轮执行。

## 输入、产物和原始等待

Root 独立核对最终九个 Task 源文件与 Git `e2aa59e`、当前文件、R1 host/Linux snapshots、源码归档内容一致。新 host 明确 darwin/arm64/CGO1，GoList 与 test 都带 race；实际 895 packages 的 7,491 listed Go/asm/header/embed/generated-main 输入与 pre/post filename/SHA 精确匹配。R1 Linux 明确 arm64/CGO0/non-race，894 packages 的 7,519 listed 输入同样精确匹配。

R1 Linux ELF 仅编译，未执行：99,607,744 bytes，SHA256 `5a7ca17735e0d61969f13fad240c8b9229784bc0418522378a6c2162295c6e3d`。原 build session 94271 实际 Wait exit0；Root 核对 ELF 类型、Go1.25.6/build flags 和实际 hash。R1 源码归档 SHA256 `d7208611bbf9642bf1c69c32ffa82725acbbb45c7eb52617bdb6e31ccd821b05`。

原 402fbc0 Linux 的 7,518 listed 输入完整绑定和 ELF 仍保留：99,603,386 bytes，SHA256 `bedab1f72d7480bed414a7f9c9fba13a02a02538beeaca452be7fef233abbfc8`，原 build session 5885 实际 Wait exit0；未执行。原源码归档 SHA256 `68c537f42bc0144965cd76026d30ed55663ff63b50655ff5db6c43ca822c222c`。新产物没有覆盖或追溯补证旧产物。

原 host 历史证据有限制：plain GoList 有 7,485 distinct listed inputs，旧 SHA 集合只捕获 7,417，漏 cached generated main 1、asm 60、header 6、embed 1；且原 plain GoList 与 race test 条件不同，不能称完整 race 闭包。八个原 Task Go 文件已匹配，但未补造旧 prehash，也未重跑矩阵制造历史证据。报告和机器 binding 已明确这一限制；R1 的新匹配条件/完整输入只证明新一轮。

测试和构建的原始 session/Wait 已保存，无运行进程或 fixture，无补 Wait。HTTP/TLS fixture 不替代真实 kubelet、Linux 执行、etcd 当前权限或物理终止证明。后续整链路仍须验证 Backend 的 durable intents/native guards、真实 Supervisor/observer、同步完成、跨 claim 恢复、精确清理及原子释放。
