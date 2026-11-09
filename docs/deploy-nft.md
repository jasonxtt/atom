# 部署：nft、策略路由和拓扑

第 4 步联调时写成，第 7 步的安装器按这里生成规则。设计依据见 [design.md](design.md) 第 9 节；这里是具体规则。

## 1. 导进 sing-box 的流量

只有两类目标会被导进 sing-box，其余流量 VM 一概不碰：

| 目标 | 来源 | TCP | UDP |
|---|---|---|---|
| FakeIP 网段（默认 `28.0.0.0/8`） | `settings.fakeip_range` | redirect → `redirect-in` 7892 | TProxy → `tproxy-in` 7893 |
| IP 规则网段（只算 IPv4） | 启用的 `ip_cidr` 规则 | 同上 | 同上 |

- 两类网段放在 nft 表 `inet atom` 的两个集合里：`fakeip4`、`iprule4`。规则文件里两个集合都是空的，由 atomdns 填（第 4 节）。
- 只看从局域网网卡进来的包（`iifname`）。目标是本机地址的包一律放过（`fib daddr type local return`），所以即使有人写了一条覆盖 VM 自己地址的 IP 规则，客户端问 VM 的 DNS（53）、管理 API（9099）也不会被导进 sing-box。
- **sing-box 自己发出的流量不会被导回来**：nft 只挂在 `prerouting` 上，本机进程（sing-box 的出站、atomdns 的上游查询）发出的包走 `output`，不经过这两条链。所以不需要给 sing-box 设 `routing_mark` 再在 nft 里排除。代价是本机进程访问 FakeIP 不会被处理（本机不需要：atomdns 的国外 DNS 走 7890，见 design.md 7.1）。
- IPv6：v0 不做 IPv6 FakeIP；IPv6 的 IP 规则只对 7890 入站有效（sing-box 的路由规则），nft 不导 IPv6。

## 2. 端口、标记、路由表

| 项 | 值 | 来源 |
|---|---|---|
| redirect 端口 | 7892 | `atom_rules.yaml` 的 `singbox.redirect_port` |
| TProxy 端口 | 7893 | `singbox.tproxy_port` |
| fwmark | `0x233` | 规则文件；只用于 TProxy 的策略路由 |
| 路由表 | 233，内容 `local 0.0.0.0/0 dev lo` | 策略路由 |
| `ip rule` 优先级 | 9233 | 策略路由；在 `main`（32766）之前 |

端口改了（`atom_rules.yaml` 不对用户开放，只有开发时会改），规则文件要跟着改。

## 3. 规则文件

### 3.1 两种形态都要的：`/etc/atom/atom.nft`

`@LAN@` 换成局域网网卡名，带引号，多个用逗号隔开，比如 `"eth0"` 或 `"ens34", "ens35"`。

```nft
#!/usr/sbin/nft -f
# atom: send FakeIP and IP-rule destinations from the LAN to sing-box.
table inet atom
delete table inet atom
table inet atom {
	# Filled by atomdns (`nft -f`, one transaction) at start and after each application.
	set fakeip4 {
		type ipv4_addr
		flags interval
		auto-merge
	}
	set iprule4 {
		type ipv4_addr
		flags interval
		auto-merge
	}

	# TCP: redirect to sing-box redirect-in (7892).
	chain prerouting_tcp {
		type nat hook prerouting priority dstnat; policy accept;
		iifname != { @LAN@ } return
		fib daddr type local return
		ip daddr @fakeip4 meta l4proto tcp redirect to :7892
		ip daddr @iprule4 meta l4proto tcp redirect to :7892
	}

	# UDP: TProxy to sing-box tproxy-in (7893); the mark sends it to table 233 (local).
	chain prerouting_udp {
		type filter hook prerouting priority mangle; policy accept;
		iifname != { @LAN@ } return
		fib daddr type local return
		ip daddr @fakeip4 meta l4proto udp tproxy ip to :7893 meta mark set 0x233 accept
		ip daddr @iprule4 meta l4proto udp tproxy ip to :7893 meta mark set 0x233 accept
	}
}
```

- 开头的 `table inet atom` + `delete table inet atom` 让文件可以重复加载：每次都整表替换，不会叠出重复规则。重新加载会把两个集合清空，所以加载后要让 atomdns 重新填（第 4 节）。
- 集合用 `flags interval; auto-merge`：IP 规则里互相重叠的网段（比如 `91.108.0.0/16` 和 `91.108.4.0/22`）会合并，不会报冲突。
- 链名不能用 `redirect`、`tproxy`（nft 的关键字），所以叫 `prerouting_tcp` / `prerouting_udp`。

### 3.2 只有双网卡网关要的：`/etc/atom/atom-gateway.nft`

```nft
#!/usr/sbin/nft -f
# atom, dual-NIC gateway only: everything else from the LAN is forwarded and NATed out of the WAN.
table ip atom_nat
delete table ip atom_nat
table ip atom_nat {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		iifname { @LAN@ } oifname "@WAN@" masquerade
	}
}
```

`@WAN@` 是外网网卡名（不带引号，文件里已经有）。同时要开转发：`net.ipv4.ip_forward = 1`（写进 `/etc/sysctl.d/90-atom.conf`）。

这份文件不带防火墙：`forward` 链默认放行。VM 本身就是网关时，WAN 侧进来的连接要不要拦，由用户现有的防火墙决定；安装器只提示，不替用户加。**[建议]**

### 3.3 策略路由（两种形态都要）

```sh
ip rule add fwmark 0x233 lookup 233 priority 9233
ip route replace local 0.0.0.0/0 dev lo table 233
```

`ip rule` 重复添加会叠出多条，安装器先 `ip rule show | grep -q 'fwmark 0x233 lookup 233'` 判断。

单网卡旁路由**不需要** `ip_forward`：redirect 和 TProxy 都把包交给本机的 socket，不经过转发。联调时单网卡形态是在 `ip_forward=0` 下测通的。

## 4. 集合由 atomdns 维护 **[定]**

- 配置：`atom_rules.yaml` 的 `singbox.nft_table: atom`（产品配置里写死）；`singbox.nft` 是命令，默认 `[nft]`。没写 `nft_table` 就不碰 nft（测试配置）。
- 时机：每次 sing-box 应用成功之后（FakeIP 网段和 IP 规则只会随应用变化，所以只改域名规则、改名这类不会动 nft），`POST apply` 之后，以及每次启动和重新加载配置时（不管 sing-box 要不要重新应用）。
- 内容：取自正在运行的 sing-box 配置文件 `config.json`（应用失败回滚后就是旧配置），读不到时才用 atom 的状态。
- 做法：写一个临时脚本，`nft -f` 一次执行，两个集合都先 `flush` 再 `add element`，是一个事务，中间状态看不到。只写 IPv4 的 IP 规则网段，去重。
- 失败（表没装、nft 不在 PATH 里）不回滚规则的改动：sing-box 已经按新规则在跑，只记日志，`GET status` 的 `nft.sync` 是 `{at, ok: false, error, sets}`。成功时 `ok: true`，`sets` 是写进去的内容。`nft.table` 给出表名。
- 更新在应用锁里做，两次修改的 nft 更新不会乱序。

所以服务的顺序是：先加载规则文件和策略路由，再启动 atomdns。安装器的写法 **[建议]**：atomdns 的单元里

```ini
ExecStartPre=/usr/sbin/nft -f /etc/atom/atom.nft
# 网关形态再加一行：
ExecStartPre=/usr/sbin/nft -f /etc/atom/atom-gateway.nft
ExecStartPre=/usr/lib/atom/policy-route.sh
```

这样 atomdns 每次启动都先把表整个重新加载，然后自己把集合填上。手动重新加载了规则文件，要重启 atomdns（或者 `POST /api/v1/atom/apply`）让它重新填集合。联调用的就是这种写法（`net-up.sh` 做了这三件事）。

## 5. 单网卡旁路由

```text
客户端 ──默认网关──> 主路由 ──静态路由 FakeIP 网段、IP 规则网段──> VM（atomdns + sing-box）
   ^                                                              │
   └────────────────────── VM 直接回包（同一网段）─────────────────┘
```

- 客户端的 DNS 指向 VM（DHCP 下发）。
- 主路由加静态路由：**FakeIP 网段和每条 IP 规则的网段**都指向 VM。只加 FakeIP 网段的话，IP 规则只对 7890 的流量有效。IP 规则改了，主路由的静态路由也要跟着改（安装器和 WebUI 要提示）。**[建议]**
- `@LAN@` 是 VM 唯一的网卡。
- **没有实测的风险**：去程经过主路由、回程由 VM 直接发给客户端，主路由只看到一半的连接。主路由的转发链如果有"丢弃 invalid 状态"的规则（RouterOS 的默认防火墙就有），可能把后续的包丢掉。处理办法二选一：在主路由的转发链里，对目标是 FakeIP 网段和 IP 规则网段的包，在 drop invalid 之前放行；或者用 DHCP option 121（无类别静态路由）把这些网段的路由直接下发给客户端，客户端就直接发给 VM，不经过主路由。联调是用 netns 模拟的（第 7 节），客户端上直接加了到 VM 的路由，相当于第二种办法，没有经过真实的主路由。

## 6. 双网卡网关

```text
客户端 ──默认网关──> VM 的 LAN 网卡 ──┬─ FakeIP / IP 规则网段 → sing-box → 节点
                                       └─ 其他 → 转发 + NAT → WAN 网卡 → 上级路由
```

- LAN 的**全部**流量都经过 VM，安装器必须明确告诉用户（design.md 9.1）。
- 规则：3.1 + 3.2 + 3.3，`ip_forward = 1`。
- 和 xray 那台机器（10.0.2.1，只读参考过）的区别：xray 把 LAN 进来的**所有** TCP/UDP 都导进 sing-box（只放过目标是本机的包），本机流量在 `output` 链按目标打标记再导回去，并用 mark `0xff` 排除 sing-box 自己的流量。atom 只导两个集合里的目标，其余照常转发，所以不需要 `output` 链和排除标记。xray 的 TProxy 标记是 `0x1`、路由表 100，atom 换成 `0x233`、233，避免和已有配置撞上。

## 7. 联调实测（2026-11-09，mosdns-rust，临时环境）

环境：

- 网关：mosdns-rust 的根 namespace，跑 atomdns（调试版）和补丁版 sing-box（`e25694ea`）。两个都是临时的 systemd 单元（`atom-e2e.service`、`atom-sing-box.service`，单元文件在 `/etc/systemd/system/`，二进制在 `/root/atom-step4/bin/`），没有替换系统里的任何程序。atomdns 的单元带 `ExecStartPre=net-up.sh`（加载规则和策略路由）。
- 节点：本机另一个 sing-box（ss 20001 / vless 20002 / socks 20003），看它的日志判断连接走了哪个节点；出口是测试机自己直连，翻不了墙，所以只验证分流，不验证访问效果。
- 客户端：两个 network namespace，各用一对 veth 接到网关。
  - `cli1`（单网卡旁路由）：只有到 `28.0.0.0/8` 和各个 IP 规则地址的路由指向 VM（代替主路由的静态路由），没有默认路由；VM 上 `ip_forward=0`。
  - `cli2`（双网卡网关）：默认路由指向 VM，VM 的 LAN 是这对 veth，WAN 是 `eth0`，`ip_forward=1` 加 NAT。
  - 注意：netns 里的程序解析域名走的是宿主的 systemd-resolved，不是测试网关。脚本里一律先 `dig @网关` 再 `curl --resolve`。
- 脚本和输出在共享笔记目录 `step4-e2e/`（`run.sh` 到 `run4.sh`）。

结果都符合预期，详见 design.md 第 11 节第 4 条。单网卡和网关两种形态都实测过；第 5 节的主路由风险没有实测条件。

## 8. 已知限制

- **补偿时重载失败，nft 和 sing-box 不一致**（codex 第 3 轮复审指出，决定记为限制、不改代码）。
  - 触发条件：要同时出现两次失败。先是一次修改已经应用到 sing-box，但保存 atom 的文件失败；接着补偿时旧的 `config.json` 写回成功，重载命令却失败了，没能让 sing-box 换回旧配置。
  - 表现：sing-box 还在跑新配置，nft 集合却按写回的旧文件更新了。新 FakeIP 网段或新 IP 规则的流量不再被导进 sing-box。接口返回 500，错误里写"已写回旧配置，但重载后没有确认"；`GET status` 的 `singbox.in_sync` 显示不一致。
  - 恢复：再应用一次，任意一次成功的修改都行，或者 `POST /api/v1/atom/apply`。应用成功后 sing-box 和 nft 都回到 atom 的状态。

## 9. 卸载

```sh
nft delete table inet atom
nft delete table ip atom_nat 2>/dev/null
ip rule del fwmark 0x233 lookup 233 priority 9233
ip route flush table 233
```
