# Pending cleanup 固定视图分页切片验证

本切片增加私有的80条固定 revision 分页，供后续自动消费者使用。它不执行 Task，
没有接入 actor、历史回填或 mounted 自动模式，也没有完成整个 pending-worker Task。
默认 main 仍使用 Redis，大 N 性能和生产切换尚未验收。

## 实现与边界

读取复用原 beginDuePage 的 session、partition、Range share、实际 workerCall 和游标
一次消费规则；不新增会话、租约、goroutine 或公开权限载体。新游标以私有包装持有原
DueCursor，复制、跨 Backend/分区/份额及重复使用均拒绝。路由检查在扣费前完成，
每次实际读取仍支付原256KiB份额，短页、错误和压缩不会退款。

请求保持原20项 worker 比较、1次 Range 和4个失败读取，共25节点。请求128KiB、
响应256KiB预检覆盖最大原生头与整数；每页80条、记录2048字节和键1024字节上限，
保守响应模型为4096+80×(1024+2048+128)=260096字节。

固定视图中新增和删除不改变后续页。结构性头、顺序、范围、计数、修订和大小错误
使整页失效；有界但不符合规范的候选只计 Rejected，后续有效候选仍返回。续接使用
最后一条原始键，即使整页候选都被拒绝也能推进；返回记录拥有自己的字节。

已知 compaction 只有在原付费调用内实际重验原 guards 后，才能保留该 session 并允许
调用者另开视图。取消和关闭等待原 RPC、验证、Range owner 与 Relist callback 实际返回。
状态重建调用者仍须使用原 RunWorkerRelist；独立分页函数不会替调用者宣称已占 Relist。

实际请求采用原生 KEY/NONE 顺序，不使用 revision 过滤器。所选版本源码中，非零
revision 过滤器或排序选项会先取消底层 limit，再过滤/截断，因此不能用这些选项假装
服务端读取有界。见 [etcd 3.6.14 executeRange](https://github.com/etcd-io/etcd/blob/v3.6.14/server/etcdserver/txn/txn.go#L145-L181)。
这里仍没有证明服务端 Count 计算的 CPU 成本与 pending 数量无关。

## 验证

- 新接口缺失的编译 RED 已保留，没有当作行为断言已运行。
- host 三组/40子例通过1.525s，race 同三组/40子例通过2.896s，无跳过、告警或数据竞争。
- Linux arm64/CGO0 的 etcd 包 vet 通过。
- 覆盖0/1/80/81条、固定视图插入删除、坏首条/坏整页续接、结构性错误、返回字节归属、
  原始 credit 与游标身份、Relist 前后 guards、压缩失权及 Close 真实返回屏障。

以上为有界 gRPC 模型与纯 wire 验证，不是真实 etcd、物理自动执行或容量验收。
host/race/vet 分别9033/9038/9124份实际输入，全部17类 Go-list 字段、源码/工具前后、
归档成员及当前字节均独立核对；race 的实际列表和测试命令均包含 -race。
没有创建 Docker 资源，也未重放已验收物理执行矩阵。

原始日志 SHA256：host
"2a6b9363927cbc519441713cf68aa6c4021d762fdc1ca15542f0753289a5e777"；
race "ded80ef85cf776566523424fa6ddeab9a02aa27150c7a73f331c3c2634e41278"。

作者与 Root 完成注释、逻辑分段和包职责检查，gofmt及差异检查通过。编码、分页和原
生命周期保持存储包私有，未为拆文件扩大权限接口。整个 Task 的唯一完整独立审查仍待
自动消费者完成后执行。下一片为具有持久可信进度的历史回填，再接终态 pending 移除和
共享调度；pending Watch R+1、独立 worker 角色及其余生产迁移范围继续保留。
