# Helm 大整数配置渲染修复设计

**日期：** 2026-09-06

## 背景

Helm Chart 的 `config.security.maxUploadBytes` 默认值为 `2147483648`。当前 YAML/Helm 解析链将其作为浮点数处理，Deployment 中的 `SANDBOX_SECURITY_MAX_UPLOAD_BYTES` 最终被渲染为 `2.147483648e+09`。sandbox-api 使用 Go `int64` 解析该环境变量，因此默认 Helm 部署会在配置加载阶段退出。

## 方案

在 `values.yaml` 中把默认值声明为十进制字符串 `"2147483648"`，保留 Deployment 模板现有的 `quote` 输出。这样 Helm 不会在中间过程把值转换为浮点数，Pod 环境变量仍得到 Kubernetes 要求的字符串，并与 Go 配置的十进制整数格式一致。

不在模板中使用浮点格式化或强制 `int64` 转换，避免对错误的小数输入进行静默截断。运维通过 values 文件覆盖时也应使用十进制字符串。

## 验证

增加 Chart 渲染回归测试，使用默认安全配置执行 `helm template`，断言：

- `SANDBOX_SECURITY_MAX_UPLOAD_BYTES` 精确渲染为 `"2147483648"`；
- 渲染结果不包含科学计数法 `2.147483648e+09`；
- `helm lint` 继续通过。

随后在本地 kind 中完成两轮 Helm 部署：首轮验证 API 健康、普通 pool、sandbox 创建/执行/销毁；卸载后重新部署同一配置，并保持最终 release 运行供检查。

## 范围边界

本修复只处理 Helm 大整数渲染。最终运行环境保持 `workspace.mode=sync`，因为当前 Kubernetes FUSE profile 仍受 provider fault matrix 与 durable-flush release gate 阻止；不通过模板参数绕过该门禁。
