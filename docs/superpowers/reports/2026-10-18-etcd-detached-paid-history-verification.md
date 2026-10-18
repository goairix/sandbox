# detached 付费历史读取验证

主提交 `cb78621`，辅助提交 `e55bc0b`，六个文件。唯一审查员完成 Spec/Quality
限定验收，独立重新核对9489项输入及四个ELF，零差异、无阻断问题。

本组件让原七阶段 paid cursor 认证完整 detached 历史；原 local/v1 仍返回其原生
writer 输入，v2 成功后保存完整私有 proof，稳定返回 `ErrInvalidConfiguration`。
尚未连接 v2 quota writer，不代表配额归还或自动销毁已完成。

第五阶段严格 seal discriminator 分流，第六阶段把真实 Task 与 termination/provider
同次付费读取，第七阶段复用完整13点签名历史认证。与前四阶段已付费认证的 cold
reservation/publication 精确匹配，publication 的 native envelope 含 Version，
拒绝同值删除重建替换。成功后退休临时 seal/head/Task 根；完整 proof 归单 cursor。
原 owner/context/session/partition/Range 及并发 busy 检查不放宽。

Host 行为 RED `detachedquotacursorred01` 在第五阶段原 v1-only seal reader 失败；
前四阶段及六类拒绝通过。GREEN 两组、focused race 十四组及 Linux arm64 vet
通过。Host 模型没有 v2 签名权限，实际完整认证另由下述原生场景证明。

实际 Linux 场景 `run-4f15da560b884a10803680a22d11cc1b` 通过：父21.93s、
worker16.41s。正式 New 前配置真实 inventory，Acquire/Claim 后 Reserve，实际
publication 后 Settle；原 observer 签发终止证明、七 provider 槽完成并执行 owner
释放。原 once 尾部真实释放 Claim、写 Run.Joined、发布 job.done，再 Stop/Done；
使用新 session、原 partition/Range0 和自然 earned credit 执行七个逻辑付费阶段。

最终完整 cursor 经原4MiB/64-key walker计68917字节、含未来组预留36键；三次
cached refusal 不移动 bucket、context、origin 或 native operation。实际来源、
ready、occupied quota、原 receipts、无 return/journal 和 retained Joined E/S
前后不变。owner 释放删除 Owner/Control/RuntimeIndex，**并不删除 E/S**。
独立 raw 对账不计入七个逻辑阶段；未测量 wire dispatch 或 transport retries。

原 metadata worker、PID1、三成员 etcd 全部执行原 Cmd.Wait；容器 exit0、无 OOM，
按精确身份删除。日志 SHA256
`ef4049c5b0e996f170d1d97b979a48f43bcd3515a3f33cfb36ed3f49e77ff461`。
race9174、Linux vet/state9280、control9485完整17输入、工具/源码归档审计通过；
原生执行后9489个唯一输入及工具全部重新核对，无变化。共用构建/依赖缓存。

主分支新增三组 host 回归通过。另以主工作区重新构建 Linux state/control，
9280/9485项完整输入集合经双向路径映射、内容指纹、环境/标签/工具链及模块语义
比较，与上述辅助原生输入完全相同；复用该次实际 native 证据，主工作区没有重跑
native。记录为主工作区 `.superpowers/sdd/2026-10-18-etcd-detached-cleanup/`
`paid-quota-cursor-native-input-equivalence.json`。

关键注释说明历史归属、退款权限边界、临时根退休和实际 join 顺序；profile join
与原事务/所有权紧密相关，留在 etcd 包，以独立文件分责，不扩大导出权限。
真实 fixture 接缝仅限测试；生产 managed admission、v2 quota writer/consumer、
HTTP、phase 全局预算、大 N 容量及部署恢复仍开放。
