# etcd 状态协议真实集成环境

运行：`bash scripts/test-etcd-state.sh`。需要Docker Compose和当前项目Go工具链。脚本启动独立project的三个etcd 3.6.15成员，client使用Go1.25兼容的v3.6.14；镜像固定digest。端口随机分配且只绑定localhost，不读取项目.env，不连接现有状态服务。退出时清理本project容器与临时volume，保留测试退出码。

可指定测试：`bash scripts/test-etcd-state.sh -run TestStage`。除被测三成员外另起一个独立单成员foreign cluster，只用于验证endpoint误混。脚本给测试设置`TEST_ETCD_ENDPOINTS`、`TEST_ETCD_FOREIGN_ENDPOINT`、`TEST_ETCD_CONTAINERS`和`TEST_ETCD_FIXTURE_PROJECT`。全局NOSPACE和leader暂停测试在任何初始化/故障操作前核实脚本project、全部容器labels及localhost端口映射；缺少隔离fixture声明时跳过，不对外部endpoints注入全局故障。直接`go test ./...`未设置endpoints时会显式跳过真实etcd测试；跳过不表示协议已验证。

fixture使用HTTP且无auth，只用于localhost隔离测试；生产client默认要求TLS和预置identity。每个测试使用唯一authority namespace，应用Open不执行bootstrap。三个成员同主机，只验证quorum/leader/Lease/Txn语义，不宣称抗主机故障或达到生产容量。
