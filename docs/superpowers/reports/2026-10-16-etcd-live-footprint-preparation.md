# Enrolled-live 存量模型准备

本记录是独立只读源码核查，非容量实测/新实现验收。没有运行Go/native、访问环境、读取secret或修改产品代码。Root核对冷读14-key集合、终止消费者与release删除集合，作为后续既定容量测试输入；不增加迁移完成百分比。

主方案`B_live≈8KiB`仍只是待采样目标，不能作为当前enrolled实现的可信容量基线。一次无重试、单generation、单snapshot成功创建，在claim/stage guard清理后、无后续operation/task/GC时，源码实际写集合留下22个持久key：

| 当前路径组成 | key数 | 来源（internal/storage/state/etcd） |
| --- | --- | --- |
| owner/control/snapshot/placement/fence/request/intent | 7 | workspace_acquire.go:180 |
| acquisition receipt | 1 | workspace_acquire.go:207、stage.go:170 |
| dispatch/input/receipt | 3 | runtime_dispatch.go:64 |
| binding/certificate/runtime index/receipt | 4 | runtime_preparation.go:101 |
| mount intent/receipt（plain也写） | 2 | runtime_preparation.go:112 |
| publication/proof/enrollment/activation/receipt | 5 | runtime_enrolled_publication.go:113、runtime_enrolled_publication_stage.go:78 |

22是当前单路径结构计数，不是所有生命周期的固定key数，更不表示全部须永久保留。scope-global identity/restore、瞬态leased claim/guard、后续operation/task历史、各版本MVCC另算；workspace fence按distinct workspace长期存量、runtime index按每runtime摊销，不能仅凭key prefix归为全局常数。

`runtime_enrolled_publication_read.go:256`把5条publication记录/receipt与9条preparation history组成14点读取，`:269`逐条要求存在。`task_runtime_termination_authority.go:321`在本地终止/释放授权时调用这条完整冷读；`task_owner_release.go:56`在释放前消费authority。故14条至少保留到相关终止/释放授权完成，不能按创建幂等窗直接回收。release只删owner/control/runtime index并写receipt（`task_owner_release_records.go:44`），不等于整条proof GC；也不能声称release后缺index的原14点冷读仍可用。

创建证书独立保存于binding-certificate与完整publication proof内；snapshot与runtime input分别保存完整payload，原native fixture确有将同一JSON传入两者的路径，但API并未规定总是相同。若二者各为4KiB，仅裸payload已达8KiB，尚未包含22个key/value的envelope、身份/receipt/签名及其他证据。4/8/16/64KiB拒绝上限不能用作平均值；本记录没有测出新的B_live、放大率或最大N。

后续一次小规模测量应在隔离fixture走真实Acquire→Claim→Dispatch→Bind→ConsumeMount→PublishEnrolled，普通namespace分别采最小/4KiB/8KiB输入，加一组最大namespace边界，各16workspace。固定revision分页Range，分别统计每family key/value/KV-protobuf字节、重复证书/配置字节，分开publication瞬时和guard确实消失后的idle；再对一个真实终止/ownerrelease样本取前后差集。合成64样本只用于序列化分解和历史边界，不代表生产分布、稳定backend放大率、10k/100k/1m容量或1h/24h验收。既定大N测试仍须测MVCC/WAL、churn、调度/FUSE/API和真实资源成本。
