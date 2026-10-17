# 自动物理任务失败保留：真实运行验证

接续核心修复 `88786f9`（主分支 `fd92f9a`）。本片只新增两个独立真实场景和窄路由，
不重跑原矩阵，不修改物理协议、产品日志、旧断言或原时间限制。

## 两场实际结果

- mounted-01：真实TLS etcd、原PID1/observer/archive与原自动worker。原proxy只在实际
  archive-bound事务成功后扣留一次ACK；原job记录Unknown且Run实际Joined，保留永久S/P。
  同worker、同session以正常自动调度恢复为新Run，真实归档/终止/release和S归还完成。
  此后Close/Wait仍返回Unknown，并含固定账本 `automatic cleanup failures=1:`；重复
  Close结果一致，借用Kernel仍有效。实际34.36s，日志9444B，SHA256
  `9e1043f9afc2fd623ee72a28ecd68431454045ffa486e6f3773eb67922a7a44b`。
- role-03：另一个独立真实Task和原进程，正式零cap、非root的cleanup-worker自行发现，
  经同样真实ACK故障后在同PID/session恢复。settled后SIGTERM，原Cmd.Wait实际exit1；
  stdout为空，stderr为预期有界etcd deadline WARN加命令固定失败行。所有原租约已撤销。
  实际34.47s，日志9400B，SHA256
  `823c870a565b19704b3a838cc93caf8d29d6bde642a3616b19c69b1c122280a5`。
- 两场均验证两次真实TaskClaim、一次archive准备、同Task出生/E出生、不同Run；恢复不
  重复close/quiescence/archive。parent各自核对实际provider1PATCH/1DELETE/14bridge、
  archive内容/模式、settled E/Joined Run、空S，原进程/handler/IO owner全部join。
- 正常自动调度的恢复不等于“job结束后恰等30秒”：下一轮起点早于job完成，也可能由
  原legacy发现提供候选。本片没有测试wake、新session、显式Execute或逻辑时钟推进。

## 构建、隔离与审计

- Go1.25.6/Linux arm64/CGO0；初版runner build-01及vet-01为9342输入，CLI product-01
  为9134输入，逐轮完整17类输入、源码/工具每个归档成员、前后hash/current核对通过。
- 只修新role输出断言后，最终runner build-03/vet-03各9342输入、exit0；CLI产品源码
  不变。最终runner SHA256 `ca1c3e1cdda40ccdbccbc3d4383f39bbf989d86be3fa7d3798ac5166ef43ecee`，
  CLI SHA256 `3529e670a4d85d7189f2b1297eb81c74352617b890a311a846ad80126ddceb35`。
- mounted-01执行初版runner并保留对应原始构建归档；后续改动仅role断言及私有校验，
  不将mounted旧结果重标为最终runner字节执行，也不重复已通过的独立场景。
- Root分别核对实际命令、只读ELF/hash、固定镜像、前后Docker限制、raw/exit/无OOM及
  精确清理：network none/private cgroup/只读root，512MiB/2CPU/256PID；drop ALL后
  仅KILL/SETUID/SETGID/SETPCAP/SYS_ADMIN/SYS_PTRACE，NNP/default seccomp；实际worker
  仍零cap。临时盘128MiB、workspace1MiB，原容器均已移除。

## 保留的失败与范围

- role-01实际恢复与exit1已通过，但新断言错误要求整个stderr只有固定行，未容纳原client
  在真实deadline发出的WARN。role-02又暴露同一DeadlineExceeded在HTTP/2 RST路径的
  等价描述。仅改新校验：恰一条≤2048B JSON WARN及最后固定行，十个字段/重复键/来源/
  proxy TLS端点/时间/私密内容均检查，只接受已观察的两种deadline描述。
- 不静音产品logger，不放宽退出码、物理或join断言。两份失败raw及退出/清理证据保留；
  fixture单端口、simple-token、真实deadline与关闭listener诊断也完整保留。
- 人工检查注释、逻辑分段与包职责；新helper留在原controlrunner测试包，未扩大产品接口。
  本片每场仅一条Task；不构成持久32/33恢复、大N、生产Kubernetes或完整pending验收。
  同一原审查员已据本片限定复核：I2 addressed、可以关闭，No actionable findings，
  无新增C/I/M问题；I1不重开，I3仍开放，不代表完整pending或整个迁移通过。
