# etcd 文件 API 范围验证记录

本单元已范围验收。唯一 fresh reviewer 确认 spec compliant、质量 Approved，C0/I0/M1，无未解决的跨任务检查，也不要求追加执行。Task 基线 `506800a4a680328abd8a1a7b540546162d9aea1c`；源码 HEAD `9ba1fbbcb9b64a24897240fe994089aa4ff25f78`。本单元不代表整个迁移、生产部署或大量已有 sandbox 的容量验收。

## 已提交内容

| 提交 | 内容 |
| --- | --- |
| `a67a58c` | 本单元设计/计划 |
| `2f0dc3b` | 有限文件工具与普通 launcher file-operation 模式、真实文件系统测试 |
| `8656106` | 八条原 DTO 文件路由、单次原 Exec、独立有限 health/ready 接纳及 wire 回归 |
| `9ba1fbb` | 新 Linux 实际文件场景、原 fixture 最小 variant、operator 文档 |

显式 etcd HTTP 模式提供 list、list-recursive、glob、read、read-lines、edit、edit-lines、download。固定 pinned launcher 普通子进程，UID/GID/env/workdir/timeout 沿 mounted profile；单次原 Exec，原签名、fencing、续期、Wait/IO join 保持。文件内容不进入 etcd，无 raw Pod exec、Redis 回退或每已有 sandbox 的新增常驻对象。

工具仅操作 /workspace、/tmp；os.Root 约束实际解析，nonblocking open 后检查实际类型。编辑与递归扫描固定父目录 FD，再用 Openat/Renameat/Fstatat，避免 Go1.25 Root.OpenRoot 打开 FIFO 的阻塞和 Root.OpenFile 自身跟随 symlink 的语义。编辑最终 symlink 拒绝，同目录独占临时文件写全/Sync/Close/Rename/父目录Sync，不预截断；rename 后错误明确不确定且不重试。

内容上限512KiB、stdin/stdout各1MiB、stderr64KiB、扫描10000项/深度64、页1000；最终 JSON 编码扩张也计入限额。binary read/download 保留 bytes，JSON lines/name 非法UTF-8明确422。原 signed terminal/drain/exit0之后才消费严格且匹配操作的结果；unknown、超限、不完整或非零结果均不能伪成功。

业务1..32、health4、ready2独立准入，共用原关闭锁、取消登记与WG，总owner≤38。health不访问service、不消耗业务token；ready仍2秒与真实Service.Ready，服务自身满载可503；Close取消且实际等待所有lane。用户路径控制字符以明确标记的百分号编码进X-File-Path，attachment由MIME编码生成。

## 实际验证与原始失败

Root 唯一执行 Go/Git/Docker；全部源码/测试/格式/docs 使用 apply_patch。固定 Go1.25.6，每 action 记录匹配实际平台/race 的17类GoList输入、源码与工具前后SHA、执行前归档和逐regular member；Root在后续源码改变前核对当前输入。初始失败、漏选和平台跳过不包装为通过。

- helper-red-01：actual17=1376、exit1、858B，SHA `8ad4b68750ce9a537fc1b0dc0e78f13914689f767c008af92db60f0ea719bae0`；只缺少新声明。
- helper-host-01：实际8项PASS/1项FAIL，macOS在创建非法UTF-8文件名时EILSEQ，未进入产品；1153B SHA `742ceedac77f6184a4134ba920a38127217406125a671d291b587f9bbed6238d`。后续仅该Darwin条件明确SKIP，Linux断言保留并在native实际运行。
- helper-host-02/race-01：actual17=1442/1449，各9项PASS/1明确平台SKIP；均1255B，SHA `2b091518487eaffe693bf9318275b1722ab2c3a8dfd806c9b05ff42a0f6f8c93` / `ee212eb358804f4f3170e641edcbdebdfe855e56ee0fb30a638a2d63e19ea864`。覆盖真实二进制/特殊名/相对与逃逸链接/FIFO、512KiB与10000条边界、深度、分页、行与原子替换/旧fd/权限、失败不截断和编码上限。Linux vet1491/raw0通过。
- launcher-red-01仅缺fileOperation；正确host-02/race-01各2项PASS，无skip，actual2310/2317、309B，SHA `46a4903237abe5e212e7a60022609a73a5ef27bcda811f44db5e608c94ae2233` / `c2bd17465cfe25b0c8e463268e72d7924626fd5de9942781ac552d19d83e4232`；Linuxvet2433/raw0。较早host-01因Root正则漏选新wrapper，只运行旧fixedmode，不属于wrapper验证。
- gateway-red-01：actual8925、exit1、7960B SHA `7cfa57e286b2fe5a7bc08f50065db1bf9cbe947c648f3782a49bdf356fdeead8`；签名/公开Verify夹具和真实不可用ready两项PASS，其余七项因files501、ready挤占和第五health无上限而预期FAIL。
- 新文件/probe与共享路由关闭变化后的全部23项gateway：host/race均PASS、无skip，actual8926/8931，各7927B，SHA `579c9b14f56d1126ebbc60f0e024fb512f983793f5ab7b92d8b85b40fb46ed57` / `b8f36cc1514dcf3cb7d2dbe37e0d46db4532635536e231ecb7aa388c071e0de9`。真实socket、全部八DTO/描述符/单Exec、严格无效果拒绝、错误/unknown、不可信终态、超限取消与实际join、特殊header、三lane满载与Close均覆盖。Linuxvet9011/raw0。

## Linux 实际文件场景

仅执行新的 `TestFileAPINative` / `SANDBOX_FILE_API_NATIVE=1`，实际PASS2.06秒、无skip；原日志7415B，SHA `762b2004349d1b0b8b1d7d49c4e827ae13b68a62cfa9f543bdd1c7a570a79f57`。实际 TLS-etcd3.6.14、正式 launcher PID1、原 service/HTTP/bridge；非特权UID/GID1000及控制配置不可读，真实Linux非法文件名422、全部八文件路由/二进制/空白与末行/两类编辑/最终下载通过。3个原Create交换加11次原Exec，共14个健康bridge，保持16上限；全部原Wait0/IO已join，正式PID1Wait0，etcd原SIGTERM已消费。raw中的TestEnrolledProfileNativeExec仅是必需的正式PID1 bootstrap helper，不是旧矩阵重跑。

Runner actual17=9183，SHA `545aaf46aa60901ee6d6a3e7d256caa9325a293432a6792b74ea6e560d3c7c2b`；launcher2034，SHA `d764f38858b57e821ed3c8e62f4ca31b087557bbcecc9621133968dec0067044`。均真实ELF64 AArch64、Go1.25.6/linux/arm64/CGO0/nonrace、构建日志0B；Root运行前后核对实际源输入、工具、归档、binary/buildinfo。新native Linuxvet9183/raw0通过。外部etcd fixture SHA `9076ea9d5662fdade520a63aa7e312fcde5a4fb7d50a5515d37896204f1c0f15`。

实际容器 `66d2bd0ab9f5499003031165d3e82a5d8a1fb090e7eede44e8184f8c52af93c7` attach/container Wait0、无OOM；networknone/privatecgroup/privatepid/readonly/512MiB2CPU256PIDs/固定cap及NNP/tmpfs、3个只读bind已核对。环境为2个显式值加5个镜像默认值，空proxy值不含宿主凭证。已精确删除、原label inventory为空；wrapper末尾postchecks已到达并核对。

common-port/simple-token警告及SIGTERM后server-closed日志完整保留并归类，不称pristine。这里没有故意断开的bridge；所有14个均健康退出。受控KubeAPI fixture不能代替真实kubelet/CRI或生产rollout。

## 审查与后续

唯一 reviewer 一次读取完整 immutable diff，按具体风险核对原终态/unknown/严格JSON与被截断的Close函数，独立核对原始日志SHA、平台skip、所有实际结果与nativeinspect；没有重新跑Go/Docker或旧矩阵。没有Critical/Important或未解决的CannotVerify；Minor仅为保留的etcd配置/退出日志噪声，转最终整分支维护整理，不延长本单元或重放已验收测试。

上传/分片仍501，内容512KiB限制不是大文件传输完成；其他API/Pool/Docker、分区/FUSE/collector、长期回收与证书、容量、部署恢复和Redis移除继续进行。默认启动仍使用legacy/Redis；整体生产切换0%。接续大文件传输时不能把1MiB原执行stdin直接扩大到2GiB并全量分配。
