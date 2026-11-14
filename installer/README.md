# atom 安装器

设计见 [../docs/design.md](../docs/design.md) 第 9.2 节，nft 规则见 [../docs/deploy-nft.md](../docs/deploy-nft.md)。

## 用户怎么装

支持 Debian 12+ / Ubuntu 22.04+（systemd）和 Alpine 3.19+（OpenRC），x86_64 或 aarch64，不支持 Docker/LXC 容器。脚本按架构和系统选包。

```bash
curl -fsSL <install.sh 的地址> | sudo bash
```

Alpine 上（脚本是 bash）：

```bash
apk add bash curl && curl -fsSL <install.sh 的地址> | bash
```

脚本下载配套包（先 GitHub，慢或不通时依次换加速前缀）、校验 sha256、解到 `/opt/atom/pkg/`，起安装向导并打印地址：

```text
atom 安装器已就绪，请在浏览器打开：
  http://10.0.0.77:9996/
```

浏览器里依次是：环境（53 端口被占时有"停用"按钮）→ 网络（单网卡旁路由 / 双网卡网关、面板端口，默认 80，可选的面板用户名和密码，留空就不需要登录）→ DNS → sing-box（FakeIP 网段、mixed 端口、可选导入节点）→ 安装。装完自动跳到面板。

已经装过的机器上再粘贴同一条命令就是升级；降级用 `curl -fsSL <install.sh 的地址> | sudo bash -s -- --force`（参数传给 `atom-installer upgrade`）。

```bash
/opt/atom/bin/atom-installer uninstall          # 卸载，数据（规则、节点、设置）保留在 /opt/atom/data
/opt/atom/bin/atom-installer uninstall --purge  # 连数据一起删
```

脚本的环境变量：`ATOM_MIRRORS`（加速前缀，空格分隔，设成空串就只直连）、`ATOM_REPO`（owner/name）、`ATOM_PKG`（本地包文件或地址，代替下载）、`ATOM_SHA256`（包必须有的 sha256，`skip` 关闭校验）。

## 装到哪里

```text
/opt/atom/
  bin/      atomdns  sing-box  atom-installer
  etc/      atom.nft  atom-gateway.nft（网关形态）  install.json  changes.json
  lib/      policy-route.sh
  data/     配置根（atomdns -d）：config_custom.yaml  sub_config/  webinfo/  sing-box/  ui/main/ …
  backup/   升级前的备份（留最近 3 份）
```

OpenRC（Alpine）上还有 `lib/reload-sing-box.sh`；停用 53 端口的服务后若改写了 resolv.conf，原内容在 `etc/resolv.conf.orig`。

`/opt/atom` 之外只有：`/etc/systemd/system/atom-dns.service`、`atom-sing-box.service`，以及用"停用系统 DNS 占用"按钮时写的 `/etc/systemd/resolved.conf.d/atom.conf`（和把 `/etc/resolv.conf` 的链接指到上游列表）。OpenRC 上服务是 `/etc/init.d/atom-dns`、`atom-sing-box`（supervise-daemon，`start_pre` 建立 nft、策略路由和 `ip_forward`），日志在 `/var/log/atom/`（卸载时删）；"停用"按钮对 OpenRC 服务是 `rc-service stop` 加 `rc-update del`，卸载时加回原来的运行级别并启动。nft 表 `inet atom`（网关形态再加 `ip atom_nat`）、`ip rule` 和 `ip_forward` 是内核运行时状态，由 `atom-dns.service` 的 `ExecStartPre` 在每次启动时建立。`changes.json` 记录安装器改过的系统设置，卸载时按它还原。

## 升级时哪些文件被覆盖

覆盖：程序、单元、nft 规则、`config_custom.yaml`、`sub_config/`、`rule/atom_catalog.json`、`ui/main/`。保留：`webinfo/`（规则、节点、设置）、每日更新下载的规则和名单、缓存。新版本新增的数据文件会补上。升级后 atom-dns 起不来（或 sing-box 没有处于最新配置）就自动恢复备份。

## 开发

```bash
go vet ./... && go test ./...        # 纯函数的测试，Mac 上就能跑
scripts/build-package.sh --version 0.1.0 --atomdns-dir DIR   # 一次出四个包、install.sh、SHA256SUMS
```

DIR 里是 atomdns 的发布产物 `atomdns-linux-<变体>.tar.gz` 和 `atom-webui.tar.gz`；包与变体对应：amd64←`amd64`、arm64←`arm64-gnu`、amd64-musl←`amd64-musl`、arm64-musl←`arm64`。sing-box 和 `atom-installer` 由脚本按架构编。`install.sh.in` 是模板，`build-package.sh` 把版本和四个包的 sha256 写进去。包里的 `atom-installer` 是 Go 标准库写的单个静态文件，向导页面内嵌在里面（`web/index.html`）。
