# Enrolled Kubernetes profile 验证记录

本单元已范围验收。唯一完整审查的I1经原作者补测、同reviewer限定R1复审解决，Approved，无新增问题；M1原日志保留。main仍使用Redis，生产切换0%；功能代码约55%/整体交付约40%的口径暂不因组件提交数上调。

## 交付

共享原canonical v2公开配置decoder；纯`BuildEnrolledPlainPod`与显式`sandbox-enrolled-plain/v1`严格profile；固定正式`serve-provisioned`仅root/PID1以目录FD/NOFOLLOW/O_EXCL建立原受保护配置，再执行原serve/BootstrapPID1。精确五volumes、固定绝对launcher命令、readonly clock/socket HostPath和root管理/非零用户身份隔离。镜像content digest由新静态ContractDigest绑定原Root-signed Birth/activation；结构自洽不能替代权限。

observer和cleanup无条件先调用共享profile selector，未知marker/保留字段缺marker拒绝降级；Inspect只给诊断，消费者仍核对原verified tuple。observer仅增加临时Binding，两原verified activation赋值接点，persisted journal wire不变；cleanup原全PodSpec hash和UID/RV条件写不变。已知Priority0/PreemptLowerPriority admission defaults只在copy归一化，两个NoExecute300s toleration固定。

源码BASE`e182a9795ceb188d83240bd2b32f1fcbdbaa1c2f`，五个小源码提交和一项审查补测：

- `c38a4ccbf6cf3985b17bdafd8918186b62385294`：共用canonical config迁移与专用验证脚本。
- `e7b2e307a1b26c3b7ece982dbdc1ceccff4667cd`：严格profile、镜像合同和纯builder。
- `575b714a90a2bb623feb60ccb4942b71083b0ef2`：两个原消费者完整tuple接线。
- `a702c20f4e8b7bffb2d8dbeefcf3a49362e39353`：正式PID1保护文件provisioning。
- `d92fb8581c9de427260fd5bb2ad3a2bba7703cdf`：真实Linux mount/正式CLI组合验证。
- `cf101442720c95c727a90687168f699a4366a5a6`：两消费者结构自洽image/config替换的独立回归，仅测试。

## 已核对验证

八个原预期RED保留：config、builder、profile mutation、admission defaults、image contract、observer、cleanup、provisioning。对应限定host/race GREEN及affected vet逐阶段通过。另有一次build前Bash quoting预检失败，修正后执行；并非Go/native失败，原作者报告已披露，Root未独立看到该次原始stdout。不同阶段快照未拼作最终同源全矩阵。

唯一native `profile-v1`：正式CLI实际PID1、实际原ELF、初始0755目录收紧0700/config0600和真实readonly socket/clock bind mounts；真实独立observer/SO_PEERCRED/signed clock→bootstrap activation→managementTLS publication，原verifier验证ready（1865字节，digest`242ed0797ad0f9a6e360ce8818edd749abdea8aa22c101bd7384a69e71d87759`）。同一个生产builder Pod结构由observer实际GET消费及cleanup Prepare接受；不含cleanup Apply/死亡/owner释放主张。

三个独立正式CLI负例stale-config/foreign-entry/nonroot均到达CLI指定拒绝并原Wait exit1、保留旧数据；正例原CLI Wait0/cgroupempty。observer Close/Serve、clock Serve实际join；etcd原SIGTERM Wait、attach/container原Wait0/0。exact container`4e0e293d84fd5a809a8541eb30ad20d7a326702539749f47e43d0add0a351d8d`删除，run-label inventory empty，final exit0/noOOM。原run session55407实际退出0。512MiB/2CPU/256PID、networknone/private/nohostPID/readonlyroot/NNP和原cap白名单保持；fresh never-started etcd export精确清理，未读取旧OWN产物。

Root独立native输入/归档审计8c253e/original96551/final9464b6退出0：runner994包/9067inputs、launcher172包/1997inputs，实际GoList全17字段pre/post filenames+SHA、每一实际source archive成员/ELF/script/current source精确匹配。runner ELF SHA`6bb8713382f744626f48e980ba181842c62bcde011029be11c7e6c839c0f32bf`，launcher SHA`8583d309fb8d6eb673b0831bff07259f6fd21a6ce0cfbf817689a08160cc3aab`，script SHA`8f664bd9851f5aa2e0bc5256bba8e7106c8fdb4a8c28971e53dfb3cb9c86e03c`。COPYFILE_DISABLE=1的新归档无AppleDouble extras。最后Linux controlrunner vet退出0、9067actual inputs匹配；Root独立run/export精确inventory为空。

R1只补两个新top selectors，未修改产品逻辑、未人为制造RED、未重跑native/旧矩阵。每个先用真实消费者接受原Pod，再DeepCopy同步修改image/annotation/canonical config.ContractDigest/env/configSHA，先证明Inspect接受，再由不变原tuple拒绝。observer要求精确activation mismatch；cleanup用新server/client和Prepare(nil)避免旧descriptor hash前置拒绝。四host/race日志原样为ok、两个vet日志为空，原session11436/69707/57941/70930/99581/95226均退出0。

Root独立7fd40c/original35534/finalf3ba94核对R1六epoch actual17 pre/post/current：observer host/vet7206、race7217；cleanup host/vet7520、race7531，环境darwin/arm64、host/vetCGO0/raceCGO1/-race匹配。Rootf4ba2d逐份读原日志和23条R1 source SHA，manifest SHA`8c6a3bb4b0092e09368826368b753a8ffabdabc53b7147ab84d474da523ed206`匹配。原native/最终manifest保留为d92fb85快照；两测试后变化不重标为R1 same-source native。

## 审查和限制

唯一fresh完整Taskreview读5源码提交/23路径的131875字节diff，SHA`15fb9dcc48332fd82488c4dc9659388ed887c3f12de82522ed6778a96bcbb6c1`，结论C0/I1/M1。I1为上述明确缺失的安全回归，现有产品已有trusted contract比较，未证实绕过；已由原作者补测，same reviewer仅复审15082字节R1 diff，SHA`c4560ef4662241ea233e7157f6196b08a9a0046f20033120966b52b31c0da0ed`，确认I1 ADDRESSED、无新增breakage，R1 Approved。Root读取最终复审报告后范围验收，后续不重开本单元。

M1：原etcd共享HTTP/gRPC端口/simple-token启动警告，及SIGTERM停机时HTTP/tcp/gRPC closed错误保留；原PASS/Wait0/0/noOOM/精确清理支持非阻断fixture噪声，不称pristine、不滤日志、不为消除噪声重跑。

Git由Root exactpaths/expectedHEAD/common→OPEN OWN fcntl锁、小步执行，branch/HEAD/initialempty/exactstaged/diffcheck/commit/postempty均核对；R1 4e219d退出0、indexempty。不碰无关Sentinel、无依赖升级/真实环境/secret/外部部署。

本记录不证明真实kubelet/CRI/OCI镜像内容或Node operator已部署：HostUsers显式true、CNI/admission额外annotations、非零PidLimit等兼容仍有限制；clock与observer真实socket的operator供给待实施。未实现Create/claim/unknownquota仲裁、真实Kubernetes bridge、API/Pool/Docker/Upload/main、长期证书/observer槽回收、分区/FUSE、容量/部署恢复或Redis移除。新代码无随既有N增长的map/timer/扫描；这不是大N容量或线上性能提升验证。
