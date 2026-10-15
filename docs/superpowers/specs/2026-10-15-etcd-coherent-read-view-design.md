# etcd一致读视图与只读复核

这是master生产接线中已定位的独立前置：现有LoadOwner/LoadControl分离且丢ModRevision，不能拼成稳定复用判断或PrepareDestroy的ExpectedControlRevision。并行于local恢复实施；不改manager/main/默认业务路径，不把元数据视图当runtime健康、业务期限或operation权限，不做generic Store适配。

## 接口

```go
func (b *Backend) ReadWorkspaceView(ctx context.Context, workspace WorkspaceIdentity) (*SandboxReadView, error)
func (b *Backend) ReadSandboxView(ctx context.Context, sandboxID string) (*SandboxReadView, error)
func (b *Backend) ConfirmSandboxView(ctx context.Context, view *SandboxReadView) error
func (view *SandboxReadView) Records() SandboxReadState
func (view *SandboxReadView) Revision() int64
func (view *SandboxReadView) ControlRevision() int64
```

SandboxReadState含Owner *WorkspaceOwnerRecord、Fence *WorkspaceFenceRecord、Placement *SandboxPlacementRecord、Control *SandboxControlRecord；Records每次返回owned复制，包括Runtime指针。Control.Snapshot只给不可变snapshot reference；本单元不加载大snapshot、不做LRU/DTO或HTTP错误接线。SandboxReadView是private self/origin封闭的只读证据，内部保留4个精确key/MOD和snapshot revision，无全局registry/cache或N actor。nil/零值/复制view或另一Backend复核必须拒绝。

## 读取语义

Workspace入口先验证绑定本storage identity。owner/fence初读仅发现sandbox ID；ID入口从placement初读仅发现partition/workspace hash。然后一次线性且identity/restore-fenced原生Txn共同读取owner、fence、placement、control，严格解码、完整关联workspace hash/sandbox/intent/generation/restore/runtime/mount等字段。发现结果不能代替最终coherent读；发现与最终关系变化返回ErrConflict，不隐式跨workspace/partition追踪替代。查询次数有界：发现1次+共同读1次，无Get逐N/Prefix全扫或自动无限CAS重试。

没有owner（workspace入口）或placement（ID入口）返回nil,nil；已有引用但对应点缺失/关系不一致不得变成成功或假notfound，按具体中途变化ErrConflict或已证实错误ErrCorruptRecord拒绝。合法无owner永久fence不是损坏。非active phase也可以返回诊断，但不授予复用。

所有点校验namespace/exact key、零lease、正create/MOD/version与revision关系、Count/More/响应数、nested header与顶层cluster/revision/identity，非canonical/unknown/oversize/corrupt对象拒绝。原有domain readers不变，不借本单元重构通用读引擎。Txn/wire预检复用既有checker：≤64nodes/256KiB，最大合法固定domain schema需列保守上界；不得给四个点各假定64KiB而超限后仍发送。无writes/leases/watch/背景goroutine。

## 复核语义

调用者可先做健康和受控时钟观察，Confirm只确认同Backend当前identity/restore和四个point MOD均未改变；一次只读Txn，不更新control/TTL、不建operation。ABA（删除重建或改回同值）必须失败。失败完整验证operator metadata以区分identity drift与业务ErrConflict，RPC/取消不能返回success。成功返回仍不是future operation或health/expiry许可。

受控时间、runtime健康、snapshot weighted LRU及API复用/操作/Destroy生产接线属于消费者后续，不把此接口称为业务迁移完成。

## 验收

新增真实三成员etcd测试：workspace/ID同point共同revision与ControlRevision；发现和共同读之间owner/placement变化拒绝；ABA、TTL/phase/runtime/control改变复核失败；absent/invalidlink/foreignnamespace/cluster/header/Range/lease/version/canonical/oversize拒绝；返回copy修改不污染原view；nil/clone/foreignbackend/cancelled复核拒绝；有限RPC数量和worst protobuf budget。仅跑新增tests，host纯函数race与real-cluster cases按来源区分；不能把未配置endpoint的SKIP称验证。无线上Redis/Kubernetes调用，Docker只用唯一testfixture并精确清理。
