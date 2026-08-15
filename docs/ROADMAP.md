# 路线图与 Milestone 规划

里程碑在 GitHub Milestones 上创建与跟踪；本文档是其镜像与说明。
当前 Milestone 列表可在仓库 Issues → Milestones 页面查看。

## v1.0 —— Windows 首发（已完成基础，待收尾）

- [x] Go 核心引擎：DoH 反污染、分流/全代理、TTL 缓存、EDNS/ECS
- [x] Windows 系统 DNS 接管/恢复（注册表 + UAC 提权）
- [x] Linux 平台层（systemd-resolved / resolv.conf）
- [x] CLI 全套命令 + 本地控制 API
- [x] 日志文件与 `truedns logs` 诊断
- [ ] GitHub 托管与 CI（当前里程碑目标）
- [ ] Releases 发布产物与 SHA256

## v1.1 —— 托盘 GUI（Windows）

- 基于控制 API 的托盘程序：一键接管/恢复、状态展示、日志查看
- 开机自启选项

## v1.2 —— 服务化与运维

- Windows 服务模式（svc），无需常驻控制台窗口
- Linux systemd 单元完善与打包（deb/rpm 可选）

## v1.3 —— 引擎能力增强

- 污染清单远程订阅与更新
- DoH3 / DoT 上游支持
- 按域名定制上游/ECS 的策略路由
- 性能与并发压测基线

## v2.0 —— 待规划

- 根据 issue 反馈滚动规划
