# true-dns

[![CI](https://github.com/highland0971/true-dns/actions/workflows/ci.yml/badge.svg)](https://github.com/highland0971/true-dns/actions/workflows/ci.yml)
[![GitHub release](https://img.shields.io/github/v/release/highland0971/true-dns)](https://github.com/highland0971/true-dns/releases)

> 仓库: <https://github.com/highland0971/true-dns> · 问题反馈与功能建议请提交 [Issue](https://github.com/highland0971/true-dns/issues) · 贡献与开发纪律见 [CONTRIBUTING.md](CONTRIBUTING.md)

**true-dns** 是一个跨平台的本地 DNS 反污染代理工具：它通过 **DoH (DNS over HTTPS)** 加密上游还原被污染域名的真实 IP（默认内置 GitHub 生态域名清单），并**接管系统 DNS 解析请求**——被污染域名走加密上游，其余请求直通系统默认 DNS（可选全代理模式）。

- Windows：修改系统 DNS 指向 `127.0.0.1`（注册表级接管 + UAC 自动提权 + 退出自动恢复）
- Linux：支持 systemd-resolved（drop-in 方式）与经典 resolv.conf 两种接管路径
- 核心引擎纯 Go 实现、平台无关，Windows/Linux/arm64 一套代码交叉编译

## 原理

国内运营商/GFW 对 `github.com` 等域名的 **UDP 53 端口应答注入伪造 IP**（DNS 污染）。本工具在本机启动一个 DNS 代理并把系统 DNS 指向它，命中污染清单的查询改走 **HTTPS 443 端口的 DoH 加密通道**（如 dns.google / cloudflare-dns.com），伪造应答无法注入；其余查询直通系统原有 DNS，对局域网/运营商内网解析零影响。

```
应用程序 ──► 系统 DNS 解析器 ──► true-dns (127.0.0.1:53)
                                    │
              ┌─────────────────────┴──────────────────────┐
              │ 命中污染清单 (github.com ...)              │ 未命中
              ▼                                            ▼
     DoH 加密上游 (race/failover)                   系统原 DNS (自动发现)
     dns.google / cloudflare-dns.com ...             └─ 可回退 DoH
```

## 功能特性

- **反污染还原**：DoH 加密上游返回真实 IP，配合 TTL 缓存（LRU + 负缓存，RFC 2308）
- **分流模式（默认）**：仅污染域名走 DoH，其余走系统 DNS；也支持全代理模式（`mode = "full"`）
- **多上游策略**：`race`（并发取最快）或 `failover`（轮转依次尝试），单点故障不影响解析
- **EDNS Client Subnet 处理**：默认剥离客户端 ECS 保护隐私，可注入伪造 ECS 调优 CDN 调度
- **系统 DNS 接管/恢复**：备份原始配置到状态文件，`Ctrl+C` 退出或崩溃后都能用 `truedns restore` 恢复
- **本地控制 API**：`/api/v1/status|flush|reload`，为托盘 GUI 预留的稳定接口
- **可配置**：TOML 配置，域名清单/上游/缓存/日志全可调，运行中可 `reload`

## 快速开始

### 下载（推荐）

在 [Releases](https://github.com/highland0971/true-dns/releases) 页面下载对应平台的二进制（附 SHA256SUMS 校验文件），Windows 用户直接运行 `truedns run` 即可。

### 构建

```bash
scripts/bootstrap-toolchain.sh   # 下载 Go 工具链到 .toolchain/ (国内镜像, 无需 root)
source .toolchain/env.sh
make build                       # 产出 dist/truedns-windows-amd64.exe 与 dist/truedns-linux-amd64
```

### Windows 使用（推荐方式）

1. 把 `dist/truedns-windows-amd64.exe` 放到任意目录（如 `C:\tools\truedns.exe`）
2. 在**管理员** PowerShell / CMD 中运行（普通权限会自动弹 UAC 提权）：

```powershell
truedns run
```

`run` 自动完成：接管系统 DNS → 刷新系统 DNS 缓存 → 启动代理监听 `127.0.0.1:53`。
`Ctrl+C` 退出时自动恢复原 DNS 设置。

3. 验证（应为真实 IP，而不是污染 IP）：

```powershell
nslookup github.com
ping github.com
```

4. 常用命令：

```powershell
truedns status     # 查看接管状态与代理运行状态
truedns flush      # 清空缓存
truedns restore    # 手动恢复系统 DNS (异常退出后使用)
```

> `truedns setup` 只接管 DNS 不启动代理（需自行 `serve`）；`truedns serve` 只启动代理不接管。
> 配置生成：`truedns config init` 写入 `%ProgramData%\truedns\config.toml`。

### Linux 使用

```bash
sudo ./dist/truedns-linux-amd64 run --config /etc/truedns/config.toml
# 或注册为服务 (见 deploy/truedns.service):
sudo systemctl enable --now truedns
```

接管方式自动检测：systemd-resolved 环境写 drop-in 并把 resolved 上游指到 `127.0.0.1`（对客户端零改动）；传统环境直接替换 `/etc/resolv.conf`（原始内容保存于状态文件）。

## 命令参考

| 命令 | 说明 |
| --- | --- |
| `truedns run` | 接管系统 DNS + 启动代理，退出时自动恢复（`--keep` 保留接管） |
| `truedns serve` | 仅启动代理，不修改系统 DNS |
| `truedns setup` | 仅接管系统 DNS（`--force` 重复接管） |
| `truedns restore` | 恢复接管前的系统 DNS 设置 |
| `truedns status` | 接管状态 + 代理运行状态（经控制 API） |
| `truedns flush` | 清空代理缓存 |
| `truedns config init/show/path` | 生成配置 / 查看合并配置 / 查看配置路径 |
| `truedns version` | 版本信息 |

全局 flags：`--config <path>`、`--log-level debug|info|warn|error`；
Windows 下 `--no-elevate` 关闭自动 UAC 提权。

## 配置文件

完整注释参考 [`config.example.toml`](config.example.toml)。要点：

- `mode`：`split`（默认，仅污染域名走 DoH）或 `full`（全部走 DoH）
- `upstreams.doh`：DoH 端点列表；`strategy` 选 `race`/`failover`；`proxy_url` 可让 DoH 走本地代理软件
- `domains.polluted`：后缀匹配（`github.com` 自动覆盖全部子域），支持 `*.x.com` 与 `*`；默认清单除 GitHub 生态外还包含 `dns.google` / `cloudflare-dns.com`（保证境外 DoH 自身获得干净 IP）
- `upstreams.system`：留空自动发现（接管前的原 DNS 会从状态文件优先恢复使用，绝不回环查询自己）
- `upstreams.fallback`：公共回退链（默认 223.5.5.5 / 119.29.29.29 / 1.1.1.1）——system 上游全部失败时兜底，修复「虚拟网卡网关无 DNS 服务导致非污染域名全部超时」一类问题
- `ecs`：`strip` 默认剥离客户端 ECS；`spoof` 可注入指定网段
- `log`：`verbose_queries = true`（配合 `level = "debug"`）打印每次查询；`stats_interval` 控制周期统计心跳（默认 1 分钟，`"0s"` 关闭），运行日志同时写入 `<配置目录>/truedns.log`，可用 `truedns logs` 查看

## 控制 API（托盘 GUI 预留接口）

监听 `127.0.0.1:5378`（可配置，可选 token 鉴权）：

```
GET  /healthz              → ok
GET  /api/v1/status        → 引擎状态 + 接管状态 (JSON)
POST /api/v1/flush         → 清空缓存
POST /api/v1/reload        → 重载配置文件
POST /api/v1/shutdown      → 优雅停止代理 (run 进程退出并恢复系统 DNS)
```

端口说明：`api.listen` 端口被系统保留/占用时自动改用备用端口（15378→…→随机），
实际端口持久化到 `<配置目录>/api-port`，CLI 与托盘 GUI 优先读取该文件发现端口。

示例：`curl http://127.0.0.1:5378/api/v1/status`

## 架构与跨平台设计

```
cmd/truedns            CLI 入口 (命令分发, 信号处理, 提权)
internal/core          DNS 引擎: 路由/EDNS/缓存/上游策略 —— 纯 Go, 平台无关
  ├─ internal/resolver   DoH 客户端 (POST+GET 回退, 校验) / 明文上游 (UDP+TCP 回退)
  ├─ internal/cache      TTL LRU 缓存 (含负缓存)
  ├─ internal/matcher    域名后缀匹配
  └─ internal/config     TOML 配置加载/校验
internal/platform      平台抽象层 (接口: Set/RestoreSystemDNS, FlushDNSCache, DiscoverSystemDNS)
  ├─ platform_windows.go   注册表 NameServer 接管 + UAC 提权 + ipconfig /flushdns
  ├─ platform_linux.go     systemd-resolved drop-in / resolv.conf 替换
  └─ platform_stub.go      其他平台桩 (新增平台只需实现 Platform 接口)
internal/api           本地控制 API (托盘 GUI 集成点)
```

设计要点：

- **引擎零平台依赖**：所有 OS 差异（注册表、resolv.conf、systemd）都收敛在 `platform` 接口后，交叉编译即得各平台产物
- **状态文件持久化**：接管前配置备份到 `takeover-state.json`，`restore` 跨重启可用；接管后的系统上游发现自动过滤回环地址、优先使用备份值，杜绝"自己问自己"
- **GUI 就绪**：CLI 内核 + 控制 API 已把托盘壳所需的全部能力（状态、flush、reload）暴露出来，托盘 GUI 只需调 API

## Windows 注意事项

- **管理员权限**：修改系统 DNS 需写 HKLM 注册表，工具会自动触发 UAC（`--no-elevate` 关闭）
- **53 端口被占用**：若启动报端口占用，常见于"Internet 连接共享 (SharedAccess)"服务，停止该服务即可；工具会打印指引
- **代理未运行时**：接管后系统 DNS 指向 127.0.0.1，若代理停止将无法解析。异常情况用 `truedns restore` 一键恢复
- **开机自启**（可选）：以管理员创建计划任务，登录时运行：
  ```
  schtasks /Create /TN "true-dns" /TR "C:\tools\truedns.exe run" /SC ONLOGON /RL HIGHEST
  ```

## 局限与说明

- 本工具解决的是 **DNS 层污染**。若目标 IP 被 TCP 阻断（部分 GitHub IP 直连不通属 IP 层问题），需要另行配置代理或优选 IP
- DoH 上游（dns.google / cloudflare-dns.com）本身在国内需可达；受限环境可在配置中设 `proxy_url` 走本地代理
- 全代理模式下，运营商内网域名/CDN 就近调度可能受影响，默认分流模式没有此问题

## 开发

```bash
scripts/bootstrap-toolchain.sh && source .toolchain/env.sh
make test      # 全量单测 (含引擎路由/缓存/EDNS/DoH 假上游集成测试)
make vet fmt
make build-all # linux-amd64 + windows-amd64 + linux-arm64
```

目录内 `testdata/config.sandbox.toml` 是沙箱联调配置（端口 5353 + 可达 DoH），
可用 `./dist/truedns-linux-amd64 serve --config testdata/config.sandbox.toml` + `dig @127.0.0.1 -p 5353 github.com` 验证。

## 路线图

- [ ] Windows 托盘 GUI（基于控制 API）
- [ ] Windows 服务模式（svc 集成，开机即接管）
- [ ] 污染清单远程更新（订阅式）
- [ ] DoH3 / DoT 上游支持
- [ ] 按域名定制 ECS / 上游的策略路由
