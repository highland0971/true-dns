# AGENTS — 自动化编码代理工作纪律

This file governs how AI coding agents (including DSH sessions) work on this
repository. It is binding for every automated change.

## 硬性规则 (MUST)

1. **任何代码改动必须关联一个 GitHub Issue**（仓库 `highland0971/true-dns`）。
   开工前若不存在对应 Issue，先在 GitHub 创建（使用 bug / feature 模板）并归属 Milestone。
2. **禁止直接向 `main` 提交。** 所有改动走 `feat/#N-*` / `fix/#N-*` / `chore/#N-*` 分支 + Pull Request，
   PR 描述包含 `Closes #N`。仓库推不动（网络/凭据）时，在本地建分支并产出完整 PR 材料（描述、提交），由人类执行推送，不降级为直接改 main。
3. **质量门槛**：`go test ./...`、`go vet ./...` 必须通过；必须完成 `GOOS=windows` 与 `GOOS=linux` 交叉编译验证。
4. **网络环境**：默认目标环境为中国大陆 —— 新增默认 DoH 上游必须国内可达；
   不得移除既有国内上游；默认配置改动属于破坏性变更，需在 PR 中说明。
5. 提交信息遵循 Conventional Commits，并在信息中关联 Issue 编号。
6. 改动公共配置/接口（config 字段、控制 API、CLI 命令）时，必须同步更新
   `config.example.toml` 与 `README.md`。

## 推荐 (SHOULD)

- 开工前阅读 [CONTRIBUTING.md](CONTRIBUTING.md) 与 `docs/` 下相关文档。
- 大改动拆分为多个小 PR；每个 PR 聚焦单一 Issue。
- 完成里程碑时提示人类创建 tag 与 Release。

## 迭代状态

- 进行中的工作由 GitHub Issue/Milestone 追踪，**不以本地 TODO 或对话记忆代替**。
- 每次会话开工时，先查看仓库当前 Issue/Milestone，确认工作归属后按流程执行。
