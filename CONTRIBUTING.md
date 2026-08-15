# 贡献与开发纪律

> true-dns 的一切开发迭代遵循 **GitHub Issue / Milestone / PR 机制**。
> 本文档是开发纪律的唯一权威来源；自动化编码代理（AI coding agent）另见 [AGENTS.md](AGENTS.md)。

## 核心原则

1. **无 Issue 不改代码** —— 任何代码改动必须先有对应 Issue（bug / feature / chore），并归属相应 Milestone。
2. **主干受保护** —— `main` 分支禁止直接推送，一切改动经 Pull Request 合入。
3. **一 PR 一 Issue** —— 每个 PR 只解决一个 Issue，PR 描述以 `Closes #N` 关联。
4. **质量门槛** —— CI 必须全绿（`go test` / `go vet` / 格式检查 / Windows+Linux 交叉构建），破坏性改动必须在 PR 中说明影响面。
5. **提交信息规范** —— 遵循 Conventional Commits：`feat` / `fix` / `chore` / `docs` / `test` / `refactor` + 简短说明。
6. **独立 Review 门禁** —— 每个 PR 必须经过独立 Review（与作者**不共享上下文**的子代理，或人类 reviewer），按 [docs/REVIEW.md](docs/REVIEW.md) 清单审查；**作者不得审查自己的 PR**；结论以 GitHub Review 评论归档，**APPROVE 且 CI 全绿方可合入**。

## 标准工作流

1. 在 GitHub 创建 Issue（使用 bug / feature 模板），并指定 Milestone。
2. 基于最新的 `main` 拉取分支：
   - 缺陷修复：`fix/#<issue>-<简述>`
   - 功能开发：`feat/#<issue>-<简述>`
   - 维护任务：`chore/#<issue>-<简述>`
3. 开发，本地保证 `make test vet` 通过（见下方环境说明）。
4. 推送分支，打开 PR（使用 PR 模板，填写 `Closes #N`）。
5. **独立 Review**：由子代理 / 人类 reviewer 按 [docs/REVIEW.md](docs/REVIEW.md) 审查，
   结论以 GitHub Review（`APPROVE` / `REQUEST_CHANGES`）归档于 PR；
   收到 `REQUEST_CHANGES` 则修复后重新请求审查。
6. **Review 通过且 CI 全绿**后，Squash 合入 `main`，删除源分支。
7. 每个 Milestone 完成时打版本 tag（`vX.Y.Z`），在 Releases 发布各平台二进制与变更说明。

## Milestone 规划

- 进行中的迭代计划以 GitHub Milestones 为准，路线图见 [docs/ROADMAP.md](docs/ROADMAP.md)。
- 里程碑粒度：每个 Milestone 应在一个版本周期内可交付、可验证。

## 开发环境

- Go 版本以 [go.mod](go.mod) 为准；无系统 Go 时执行 `scripts/bootstrap-toolchain.sh && source .toolchain/env.sh`。
- 常用命令：`make test`、`make vet`、`make build-all`。
- 沙箱/本地联调配置：`testdata/config.sandbox.toml`（监听 5353，不接管系统 DNS）。

## 网络环境约束（重要）

默认目标环境为**中国大陆网络**：

- 新增的默认 DoH 上游必须国内可达；国内不可达的端点只能作为备选（置于列表尾部）或文档建议配合 `proxy_url` 使用。
- 不得移除既有国内上游；改动默认上游属于破坏性变更，需在 PR 中说明。
- 发布二进制需附带 SHA256 校验值。

## 分支保护（严格 PR 模式）

`main` 分支已启用保护：必须通过 PR 合入、必须通过 CI 状态检查。单人开发时允许自行合入——reviewer 可为独立子代理，无需第二个人类账号——但流程本身（Issue → 分支 → PR → **独立 Review**）不可跳过：Review 环节由本文档与 [docs/REVIEW.md](docs/REVIEW.md) 强制，不以 GitHub 的 APPROVE 计数为唯一依据。
