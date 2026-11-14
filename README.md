# atom

一台 Linux 机器同时跑 DNS 和代理的分流网关。由 DNS 统一决定每个域名走哪个出口（直连、哪个节点组、屏蔽），代理核心只负责执行，不再做第二次判断。一套规则、一个面板。

```text
客户端 DNS ──> atomdns（规则表 → 真实 IP / FakeIP + 出口绑定）── EDNS0 65001 ──> sing-box
客户端流量 ──> nft（TCP redirect / UDP TProxy）──> sing-box（按绑定或 Final 选节点）──> 节点
```

## 拓扑

| 拓扑 | 接法 |
|---|---|
| 单网卡旁路由 | atom 和主路由在同一网段 |
| 双网卡网关 | atom 的 LAN 口接主路由（例如主路由的 WAN2），WAN 口出网 |

两种拓扑在主路由上要做的都一样：

1. 把 FakeIP 网段（默认 `28.0.0.0/8`）和要代理的网段路由到 atom 的 LAN IP（静态路由，或双网卡时走接 atom 的那个口）。
2. 客户端的 DNS 设为 atom 的 LAN IP（通常改 DHCP 下发的 DNS）。

到达 atom 的转发流量都进 sing-box：FakeIP 按 DNS 给的绑定走，真实 IP 按 IP 规则走，都没命中走 Final。

## 安装

Debian 12+ 或 Ubuntu 22.04+（systemd）、Alpine 3.19+（OpenRC），x86_64 或 aarch64，root，不支持 Docker/LXC 容器：

```bash
curl -fsSL https://github.com/jasonxtt/atom/releases/latest/download/install.sh | sudo bash
```

脚本下载配套包（GitHub 不通时依次换加速前缀）、校验 sha256，起一个网页向导并打印地址；在浏览器里选拓扑、配 DNS 和 sing-box，装完自动跳到面板。Alpine 上先 `apk add bash curl`，命令里的 `sudo bash` 换成 `bash`。已经装过的机器上再运行同一条命令就是升级（降级：`… | sudo bash -s -- --force`）。卸载：`/opt/atom/bin/atom-installer uninstall [--purge]`。详见 [installer/README.md](installer/README.md)。

## 仓库

| 仓库 | 内容 |
|---|---|
| [jasonxtt/atom](https://github.com/jasonxtt/atom)（本仓库） | 安装器、打包脚本、设计和部署文档；用子模块固定下面两个仓库的配套版本 |
| [jasonxtt/atom-core](https://github.com/jasonxtt/atom-core)（子模块 `atomdns/`，分支 `atom/main`） | AtomDNS 的 atom 分支：DNS、规则表、节点和节点组、生成 sing-box 配置、管理 API、WebUI。程序名 `atomdns` |
| [jasonxtt/sing-box](https://github.com/jasonxtt/sing-box)（子模块 `sing-box/`，分支 `atom/fakeip-outbound`） | sing-box v1.14.4 加 FakeIP 出口绑定补丁，见 [docs/singbox-fakeip-outbound.md](docs/singbox-fakeip-outbound.md) |

设计见 [docs/design.md](docs/design.md)，nft 和策略路由见 [docs/deploy-nft.md](docs/deploy-nft.md)，管理 API 见 `atomdns/docs/api/openapi.yaml`。

## 从源码构建

```bash
git clone --recursive https://github.com/jasonxtt/atom.git && cd atom

# atomdns 和 WebUI：用 atomdns 的 scripts/build-rust-release.sh（VARIANTS="amd64 arm64-gnu amd64-musl arm64"，Linux x86_64 上，
# 需要 Rust stable 和 Node.js），或 GitHub 上 release-build.yml 的产物：atomdns-linux-<变体>.tar.gz 和 atom-webui.tar.gz，放在同一个目录 DIR
# sing-box 和安装器由打包脚本按架构编（需要 Go）
installer/scripts/build-package.sh --version 0.1.0 --atomdns-dir DIR
```

产物在 `installer/dist/`：四个包 `atom-<版本>-linux-{amd64,arm64,amd64-musl,arm64-musl}.tar.gz`、写好版本和四个 sha256 的 `install.sh`、`SHA256SUMS`。本地包可以用 `ATOM_PKG=<包> bash install.sh` 安装。

## 许可证

[GPL-3.0](LICENSE)。atom-core 和 sing-box fork 是 AtomDNS（MosDNS）和 sing-box 的衍生作品，同样以 GPL-3.0 发布，保留各自原有的版权声明。
