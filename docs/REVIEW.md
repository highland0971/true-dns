# PR Review 清单（docs/REVIEW.md）

本清单是合入门禁的一部分：**每个 PR 必须由独立 reviewer 按本清单审查，
Review 未通过或 CI 未绿一律不得合入。**

## 审查方要求

- 与作者**不共享上下文**：优先派发独立子代理（subagent）执行，或由人类 reviewer 执行；作者不得审查自己的 PR。
- 审查基于实际 diff：子代理应在本地 checkout 拉取 PR 分支，`git diff origin/main...<分支>` 逐一过目，而非只读 PR 描述。
- 审查方**自行运行质量门**，不得信任作者声称的结果：
  - `go test ./...`、`go vet ./...`
  - `test -z "$(gofmt -l cmd internal)"`
  - `GOOS=windows GOARCH=amd64 go build ./cmd/truedns` 与 `GOOS=linux GOARCH=amd64 go build ./cmd/truedns`
- 结论必须落在 PR 评论中，形式为 GitHub Review：`APPROVE` 或 `REQUEST_CHANGES`（附具体问题清单）。

## 审查清单

### 1. 关联性与范围

- [ ] PR 描述 `Closes #N` 指向正确的 Issue，且只解决该 Issue（无夹带改动）
- [ ] 改动范围与 Issue 描述一致；超出部分要么拆新 Issue，要么在 PR 中说明理由

### 2. 正确性

- [ ] 路由逻辑（分流/全代理、缓存、EDNS/ECS）无逻辑错误
- [ ] 并发安全：共享状态有锁保护，无数据竞争、无 goroutine 泄漏
- [ ] 错误处理完整：上游失败路径可降级（SERVFAIL/回退），不留半接管状态
- [ ] 接管/恢复对称性：Windows 注册表、Linux resolved/resolv.conf 的备份与恢复严格配对
- [ ] 时间与资源：超时合理，无无界缓存/队列增长

### 3. 质量门（自行运行，不采信 PR 描述）

- [ ] `go test ./...` 全绿
- [ ] `go vet ./...` 无告警
- [ ] gofmt 无差异
- [ ] Windows + Linux 交叉编译通过

### 4. 配置与文档同步（AGENTS 规则 6）

- [ ] 公共配置字段变更同步 `config.example.toml`（注释说明默认值与语义）
- [ ] CLI 命令 / 控制 API 变更同步 `README.md`
- [ ] 默认值变更标注为破坏性变更并在 PR 中说明

### 5. 网络环境约束（默认中国大陆）

- [ ] 新增默认 DoH 上游国内可达（不可达只能作备选或文档说明需 proxy_url）
- [ ] 未移除既有国内上游
- [ ] 无硬编码境外必需直连（若必须，提供 proxy_url 配置路径）

### 6. 安全与卫生

- [ ] 无凭据/密钥/Token/内部路径硬编码（检查常量、注释、测试 fixture、日志）
- [ ] 日志不泄露用户敏感信息（查询记录默认关闭）
- [ ] 无新增依赖未说明用途；依赖版本锁定在 go.sum

### 7. 提交与流程

- [ ] 提交信息 Conventional Commits，关联 Issue 编号
- [ ] 分支命名符合 `feat|fix|chore/#N-*`

## 结论模板

```
## Review 结论: APPROVE | REQUEST_CHANGES

审查人: <子代理/人类>, 审查范围: <PR #N, 分支>, 基于 diff 逐项核对。

[ ] 关联性与范围  ...
[ ] 正确性        ...
[ ] 质量门 (自行运行结果)
[ ] 配置与文档同步 ...
[ ] 网络约束      ...
[ ] 安全与卫生    ...
[ ] 提交与流程    ...

REQUEST_CHANGES 时列出必须修复的具体问题与位置。
```
