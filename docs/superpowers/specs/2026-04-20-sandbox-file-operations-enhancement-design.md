# Sandbox 文件操作增强设计

## 概述

本设计文档描述了 Sandbox 平台文件操作功能的增强方案，主要包括四个新功能：

1. **递归文件列表**：支持递归列出目录下所有文件，带分页和深度控制
2. **按行读取文件**：支持按行号范围读取文件内容，优化大文件处理
3. **字符串替换编辑**：支持基于字符串匹配的文件编辑，类似 sed 替换
4. **按行范围编辑**：支持基于行号范围的文件编辑，类似 git diff 方式

## 背景

当前 Sandbox 的文件操作存在以下限制：

- `ListFiles` 只能列出单层目录，无法递归匹配文件（如 `**/*.go`）
- `DownloadFile` 返回整个文件，对于大文件（如日志）读取部分内容时浪费带宽
- 文件编辑需要"下载 → 修改 → 上传"三步操作，缺少原子性保证

这些限制在实际使用中（特别是 AI Agent 场景）带来了不便和性能问题。

## 设计目标

1. **向后兼容**：不影响现有接口和代码
2. **类型安全**：使用明确的方法签名，避免通用接口
3. **性能优化**：支持分页和部分读取，减少不必要的数据传输
4. **安全性**：防止路径遍历、命令注入等安全问题
5. **原子性**：文件编辑操作保证原子性，避免并发问题

## 架构设计

### 分层结构

```
┌─────────────────────────────────────┐
│         SDK Layer (Go)              │
│  Sandbox.ListFilesRecursive()       │
│  Sandbox.ReadFileLines()            │
│  Sandbox.EditFile()                 │
│  Sandbox.EditFileLines()            │
└─────────────────────────────────────┘
              ↓ HTTP
┌─────────────────────────────────────┐
│         API Layer (Handler)         │
│  POST /files/list-recursive         │
│  POST /files/read-lines             │
│  POST /files/edit                   │
│  POST /files/edit-lines             │
└─────────────────────────────────────┘
              ↓
┌─────────────────────────────────────┐
│      Manager Layer (Sandbox)        │
│  manager.ListFilesRecursive()       │
│  manager.ReadFileLines()            │
│  manager.EditFile()                 │
│  manager.EditFileLines()            │
└─────────────────────────────────────┘
              ↓
┌─────────────────────────────────────┐
│      Runtime Layer (Docker/K8s)     │
│  runtime.ListFilesRecursive()       │
│  runtime.ReadFileLines()            │
│  runtime.EditFile()                 │
│  runtime.EditFileLines()            │
└─────────────────────────────────────┘
```

## 接口定义

### Runtime 接口扩展

在 `internal/runtime/runtime.go` 中新增四个方法：

```go
type Runtime interface {
    // ... 现有方法 ...
    
    // ListFilesRecursive 递归列出目录下的所有文件
    // maxDepth: 最大递归深度，0 表示无限制
    // offset: 分页偏移量（跳过前 N 个结果）
    // limit: 返回的最大文件数，0 表示无限制
    // 返回：文件列表、总文件数、错误
    ListFilesRecursive(ctx context.Context, id string, dir string, maxDepth, offset, limit int) ([]FileInfo, int, error)
    
    // ReadFileLines 按行读取文件内容
    // startLine: 起始行号（1-based）
    // lineCount: 读取行数，0 表示读取到文件末尾
    ReadFileLines(ctx context.Context, id string, path string, startLine, lineCount int) (string, error)
    
    // EditFile 原子性地替换文件内容（基于字符串匹配）
    // oldString: 要替换的字符串
    // newString: 替换后的字符串
    // replaceAll: 是否替换所有匹配（false 则只替换第一个）
    EditFile(ctx context.Context, id string, path, oldString, newString string, replaceAll bool) error
    
    // EditFileLines 原子性地替换文件内容（基于行号范围）
    // startLine: 起始行号（1-based）
    // endLine: 结束行号（包含，1-based），0 表示到文件末尾
    // newContent: 新的内容（替换 startLine 到 endLine 的所有行）
    EditFileLines(ctx context.Context, id string, path string, startLine, endLine int, newContent string) error
}
```

### FileInfo 结构修改

将 `ModTime` 字段从 `int64` 改为 `time.Time`，与 SDK 层保持一致：

```go
type FileInfo struct {
    Name    string
    Path    string
    Size    int64
    IsDir   bool
    ModTime time.Time  // 修改：从 int64 改为 time.Time
}
```

### API 类型定义

在 `pkg/types/sandbox.go` 中新增：

```go
// ListFilesRecursiveRequest 递归列出文件的请求
type ListFilesRecursiveRequest struct {
    Path     string `json:"path" binding:"required"`
    MaxDepth int    `json:"max_depth,omitempty"`
    Offset   int    `json:"offset,omitempty"`
    Limit    int    `json:"limit,omitempty"`
}

// ListFilesRecursiveResponse 递归列出文件的响应
type ListFilesRecursiveResponse struct {
    Files []FileInfo `json:"files"`
    Total int        `json:"total"` // 总文件数（用于分页）
    Path  string     `json:"path"`
}

// ReadFileLinesRequest 按行读取文件的请求
type ReadFileLinesRequest struct {
    Path      string `json:"path" binding:"required"`
    StartLine int    `json:"start_line" binding:"required,min=1"`
    LineCount int    `json:"line_count,omitempty"`
}

// ReadFileLinesResponse 按行读取文件的响应
type ReadFileLinesResponse struct {
    Content   string `json:"content"`
    StartLine int    `json:"start_line"`
    EndLine   int    `json:"end_line"` // 实际读取到的最后一行
}

// EditFileRequest 编辑文件的请求
type EditFileRequest struct {
    Path       string `json:"path" binding:"required"`
    OldString  string `json:"old_string,omitempty"`
    NewString  string `json:"new_string,omitempty"`
    ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditFileLinesRequest 按行编辑文件的请求
type EditFileLinesRequest struct {
    Path       string `json:"path" binding:"required"`
    StartLine  int    `json:"start_line" binding:"required,min=1"`
    EndLine    int    `json:"end_line,omitempty"` // 0 表示到文件末尾
    NewContent string `json:"new_content"`
}
```

### SDK 类型定义

在 `sdk/go/types.go` 中新增对应的类型（与 API 层类似，省略 binding tag）。

在 `sdk/go/sandbox.go` 中为 `Sandbox` 类型新增便捷方法：

```go
// ListFilesRecursive 递归列出目录下的所有文件
func (s *Sandbox) ListFilesRecursive(ctx context.Context, path string, maxDepth, offset, limit int) (ListFilesRecursiveResponse, error)

// ReadFileLines 按行读取文件内容
func (s *Sandbox) ReadFileLines(ctx context.Context, path string, startLine, lineCount int) (ReadFileLinesResponse, error)

// EditFile 通过字符串替换编辑文件
func (s *Sandbox) EditFile(ctx context.Context, path, oldString, newString string, replaceAll bool) error

// EditFileLines 通过行号范围编辑文件
func (s *Sandbox) EditFileLines(ctx context.Context, path string, startLine, endLine int, newContent string) error
```

## 实现细节

### 1. ListFilesRecursive 实现（Docker Runtime）

使用 `find` 命令递归列出文件，通过管道配合 `tail` 和 `head` 实现分页：

```go
func (r *Runtime) ListFilesRecursive(ctx context.Context, id string, dir string, maxDepth, offset, limit int) ([]runtime.FileInfo, int, error) {
    // 构建深度限制参数
    depthFlag := ""
    if maxDepth > 0 {
        depthFlag = fmt.Sprintf("-maxdepth %d", maxDepth)
    }
    
    // 先获取总数
    countCmd := fmt.Sprintf("find %s %s -type f -o -type d | wc -l", shellEscape(dir), depthFlag)
    countResult, err := r.Exec(ctx, id, runtime.ExecRequest{Command: countCmd})
    if err != nil {
        return nil, 0, err
    }
    
    var total int
    fmt.Sscanf(strings.TrimSpace(countResult.Stdout), "%d", &total)
    
    // 构建分页查询
    pipeCmd := ""
    if offset > 0 {
        pipeCmd += fmt.Sprintf(" | tail -n +%d", offset+1)
    }
    if limit > 0 {
        pipeCmd += fmt.Sprintf(" | head -n %d", limit)
    }
    
    // 使用 find 的 -printf 格式化输出：相对路径、大小、类型、修改时间
    cmd := fmt.Sprintf("find %s %s -printf '%%P\\t%%s\\t%%Y\\t%%T@\\n'%s", 
        shellEscape(dir), depthFlag, pipeCmd)
    
    result, err := r.Exec(ctx, id, runtime.ExecRequest{Command: cmd})
    if err != nil {
        return nil, 0, err
    }
    
    // 解析结果
    var files []runtime.FileInfo
    for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
        if line == "" {
            continue
        }
        parts := strings.SplitN(line, "\t", 4)
        if len(parts) < 4 || parts[0] == "" {
            continue
        }
        
        var size int64
        fmt.Sscanf(parts[1], "%d", &size)
        
        isDir := parts[2] == "d"
        
        var modTimeUnix float64
        fmt.Sscanf(parts[3], "%f", &modTimeUnix)
        modTime := time.Unix(int64(modTimeUnix), 0)
        
        fullPath := filepath.Join(dir, parts[0])
        
        files = append(files, runtime.FileInfo{
            Name:    filepath.Base(parts[0]),
            Path:    fullPath,
            Size:    size,
            IsDir:   isDir,
            ModTime: modTime,
        })
    }
    
    return files, total, nil
}
```

**关键点**：
- 使用 `find -printf` 一次性获取所有元数据，避免多次调用
- 先执行 `wc -l` 获取总数，用于分页
- 使用 `tail` 和 `head` 实现分页，避免传输大量数据

### 2. ReadFileLines 实现

使用 `sed` 命令按行号读取：

```go
func (r *Runtime) ReadFileLines(ctx context.Context, id string, path string, startLine, lineCount int) (string, error) {
    endLine := ""
    if lineCount > 0 {
        endLine = fmt.Sprintf("%d", startLine+lineCount-1)
    } else {
        endLine = "$" // 到文件末尾
    }
    
    // sed -n '10,20p' 表示打印第 10-20 行
    cmd := fmt.Sprintf("sed -n '%d,%sp' %s", startLine, endLine, shellEscape(path))
    
    result, err := r.Exec(ctx, id, runtime.ExecRequest{Command: cmd})
    if err != nil {
        return "", err
    }
    
    return result.Stdout, nil
}
```

**关键点**：
- 使用 `sed -n 'start,endp'` 精确读取指定行范围
- `$` 表示文件末尾，支持读取到文件结束
- 不需要下载整个文件，节省带宽

### 3. EditFile 实现

使用临时文件 + `sed` + 原子替换：

```go
func (r *Runtime) EditFile(ctx context.Context, id string, path, oldString, newString string, replaceAll bool) error {
    tmpFile := fmt.Sprintf("/tmp/edit-%s", randSuffix(10))
    
    // 构建 sed 替换标志
    sedFlag := ""
    if replaceAll {
        sedFlag = "g"
    }
    
    // 使用 sed 替换并写入临时文件，然后原子性地移动
    // && 确保前一步成功才执行下一步
    cmd := fmt.Sprintf("sed 's/%s/%s/%s' %s > %s && mv %s %s",
        shellEscapeSed(oldString),
        shellEscapeSed(newString),
        sedFlag,
        shellEscape(path),
        tmpFile,
        tmpFile,
        shellEscape(path))
    
    _, err := r.Exec(ctx, id, runtime.ExecRequest{Command: cmd})
    return err
}

// shellEscapeSed 转义 sed 表达式中的特殊字符
func shellEscapeSed(s string) string {
    // 转义 sed 分隔符和特殊字符
    s = strings.ReplaceAll(s, "/", "\\/")
    s = strings.ReplaceAll(s, "&", "\\&")
    s = strings.ReplaceAll(s, "\\", "\\\\")
    return s
}
```

**关键点**：
- 使用临时文件避免直接修改原文件
- `mv` 命令在 Linux 上是原子操作（同一文件系统内）
- 需要专门的 sed 转义函数防止注入

### 4. EditFileLines 实现

使用 `sed` 的行号范围删除 + 插入：

```go
func (r *Runtime) EditFileLines(ctx context.Context, id string, path string, startLine, endLine int, newContent string) error {
    tmpFile := fmt.Sprintf("/tmp/edit-%s", randSuffix(10))
    contentFile := fmt.Sprintf("/tmp/content-%s", randSuffix(10))
    
    // 先将新内容写入临时文件
    if err := r.UploadFile(ctx, id, contentFile, strings.NewReader(newContent)); err != nil {
        return err
    }
    
    endLineStr := fmt.Sprintf("%d", endLine)
    if endLine == 0 {
        endLineStr = "$"
    }
    
    // 步骤：
    // 1. 删除指定行范围，输出到临时文件
    // 2. 在 startLine-1 位置插入新内容
    // 3. 原子性地替换原文件
    cmd := fmt.Sprintf("sed '%d,%sd' %s > %s && sed -i '%dr %s' %s && mv %s %s",
        startLine, endLineStr, shellEscape(path), tmpFile,
        startLine-1, contentFile, tmpFile,
        tmpFile, shellEscape(path))
    
    _, err := r.Exec(ctx, id, runtime.ExecRequest{Command: cmd})
    
    // 清理临时文件
    r.Exec(ctx, id, runtime.ExecRequest{Command: fmt.Sprintf("rm -f %s %s", tmpFile, contentFile)})
    
    return err
}
```

**关键点**：
- 使用 `sed 'd'` 删除行范围，`sed 'r'` 读取文件插入
- 通过 `&&` 连接命令确保步骤顺序执行
- 最后用 `mv` 原子性替换原文件
- 清理临时文件避免泄漏

## 错误处理

### 参数验证

在 API handler 层进行严格的参数验证：

**ListFilesRecursive**：
- `path` 必须符合 `validateSandboxPath`（防止路径遍历）
- `maxDepth` 如果指定，必须 >= 1
- `offset` 必须 >= 0
- `limit` 必须 >= 0

**ReadFileLines**：
- `path` 必须符合 `validateSandboxPath`
- `startLine` 必须 >= 1
- `lineCount` 必须 >= 0

**EditFile**：
- `path` 必须符合 `validateSandboxPath`
- `oldString` 和 `newString` 至少有一个非空
- `oldString` 需要转义特殊字符（防止 sed 注入）

**EditFileLines**：
- `path` 必须符合 `validateSandboxPath`
- `startLine` 必须 >= 1
- `endLine` 如果非 0，必须 >= startLine

### 错误场景处理

| 场景 | 处理方式 |
|------|---------|
| 文件不存在 | ReadFileLines/EditFile/EditFileLines 返回 404 错误 |
| 目录不存在 | ListFilesRecursive 返回空列表 |
| 行号超出范围 | ReadFileLines 返回空字符串；EditFileLines 返回错误 |
| 字符串未找到 | EditFile 返回错误（避免静默失败） |
| 权限问题 | 返回 500 错误并记录日志 |
| 分页边界 | offset >= total 时返回空列表，不报错 |

### 安全考虑

**Shell 注入防护**：
- 所有路径参数使用 `shellEscape()` 包裹
- EditFile 的 oldString/newString 使用专门的 `shellEscapeSed()` 转义
- 使用 regexp 验证特殊字符

**资源限制**：
- ListFilesRecursive：在 handler 层设置默认 limit（建议 1000）
- ReadFileLines：限制单次读取的最大行数（建议 10000 行）
- EditFile：限制 oldString/newString 的最大长度（建议 1MB）

## 测试策略

### 单元测试

在 `internal/runtime/docker/file_test.go` 中新增：

```go
// ListFilesRecursive 测试
- TestListFilesRecursive_BasicRecursion: 测试基本递归列出
- TestListFilesRecursive_MaxDepth: 测试深度限制
- TestListFilesRecursive_Pagination: 测试分页功能
- TestListFilesRecursive_EmptyDirectory: 测试空目录

// ReadFileLines 测试
- TestReadFileLines_FullFile: 测试读取整个文件
- TestReadFileLines_PartialLines: 测试读取部分行
- TestReadFileLines_OutOfRange: 测试行号超出范围

// EditFile 测试
- TestEditFile_ReplaceFirst: 测试替换第一个匹配
- TestEditFile_ReplaceAll: 测试替换所有匹配
- TestEditFile_NotFound: 测试字符串未找到
- TestEditFile_SpecialChars: 测试特殊字符转义

// EditFileLines 测试
- TestEditFileLines_ReplaceRange: 测试替换行范围
- TestEditFileLines_ToEnd: 测试替换到文件末尾
- TestEditFileLines_SingleLine: 测试替换单行
```

### 集成测试

在 `sdk/go/client_test.go` 中新增端到端测试：

```go
- TestClient_ListFilesRecursive: 端到端测试递归列表
- TestClient_ReadFileLines: 端到端测试按行读取
- TestClient_EditFile: 端到端测试字符串替换
- TestClient_EditFileLines: 端到端测试行范围替换
```

### API 测试

在 `internal/api/handler/file_test.go` 中新增：

```go
- 测试各个 handler 的参数验证
- 测试错误响应格式
- 测试路径安全验证
```

### 手动测试场景

1. 创建包含多层嵌套目录的测试文件结构
2. 测试大文件（10000+ 行）的按行读取性能
3. 测试包含特殊字符的文件名和内容
4. 测试并发编辑同一文件（验证原子性）
5. 测试分页在大目录（1000+ 文件）下的表现

## 实现计划

### 阶段 1：Runtime 层实现
1. 修改 `internal/runtime/runtime.go` 接口定义
2. 实现 Docker runtime 的四个新方法
3. 编写单元测试

### 阶段 2：Manager 层实现
1. 在 `internal/sandbox/manager.go` 中添加对应方法
2. 实现参数验证和错误处理

### 阶段 3：API 层实现
1. 在 `pkg/types/sandbox.go` 中定义请求/响应类型
2. 在 `internal/api/handler/file.go` 中实现四个 handler
3. 在路由中注册新的 endpoint
4. 编写 API 测试

### 阶段 4：SDK 层实现
1. 在 `sdk/go/types.go` 中定义类型
2. 在 `sdk/go/client.go` 中实现 Client 方法
3. 在 `sdk/go/sandbox.go` 中添加便捷方法
4. 编写集成测试

### 阶段 5：文档和示例
1. 更新 API 文档
2. 添加使用示例
3. 更新 CHANGELOG

## 未来扩展

### Kubernetes Runtime 支持
当前设计主要针对 Docker runtime。未来可以为 Kubernetes runtime 实现相同接口，可能的优化：
- 使用 Kubernetes API 而非 exec 命令
- 利用 PVC 直接访问文件系统

### 文件监听
可以考虑添加文件变更监听功能：
```go
WatchFile(ctx context.Context, id string, path string) (<-chan FileEvent, error)
```

### 批量操作
支持批量文件操作以减少 RPC 调用：
```go
BatchEditFiles(ctx context.Context, id string, edits []EditRequest) error
```

## 总结

本设计通过新增四个文件操作方法，显著增强了 Sandbox 平台的文件处理能力：

1. **递归文件列表**：支持深度控制和分页，满足文件搜索和匹配需求
2. **按行读取**：优化大文件处理，减少带宽消耗
3. **字符串替换编辑**：提供类似 sed 的文本替换能力
4. **按行范围编辑**：支持精确的行级编辑，类似 git diff 方式

设计遵循以下原则：
- 向后兼容，不影响现有功能
- 类型安全，使用明确的方法签名
- 安全可靠，防止注入攻击和并发问题
- 性能优化，支持分页和部分读取

实现将分五个阶段进行，从底层 Runtime 到上层 SDK，逐步完成功能开发和测试。
