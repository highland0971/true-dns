## 关联 Issue

Closes #N

## 改动说明

- 简述做了什么、为什么这么做（面向 reviewer 的上下文）

## 验证

- [ ] `go test ./...` 通过
- [ ] `go vet ./...` 通过
- [ ] 格式检查通过（`gofmt -l cmd internal` 无输出）
- [ ] Windows 与 Linux 交叉编译通过
- [ ] 手工验证步骤（如有）：

## 影响面

- [ ] 配置项变更（已同步 `config.example.toml` 与 `README.md`）
- [ ] 默认行为变更（属破坏性变更，已说明）
- [ ] 平台层变更（Windows / Linux）
- [ ] 控制 API / CLI 命令变更
- [ ] 默认 DoH 上游变更（国内可达性已确认）

## Review 记录

独立 reviewer（子代理 / 人类）按 [docs/REVIEW.md](docs/REVIEW.md) 审查后，
在本 PR 评论中归档结论（`APPROVE` / `REQUEST_CHANGES`）。合入前提：**APPROVE + CI 全绿**。

## 备注

- 其他 reviewer 需要知道的信息
