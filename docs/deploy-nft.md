# 部署：nft、策略路由和拓扑

第 4 步联调时写成，第 7 步的安装器（`installer/templates.go`）按这里生成规则，装在 `/opt/atom/etc/`（`atom.nft`、`atom-gateway.nft`）和 `/opt/atom/lib/policy-route.sh`；下面文件名里的 `/etc/atom/` 是联调时的位置。设计依据见 [design.md](design.md) 第 9 节；这里是具体规则。

## 1. 导进 sing-box 的流量

2026-11-12 改（用户定）：从局域网网卡进来、目标不是本机的 IPv4 TCP、UDP **全部**导进 sing-box，两种拓扑一样。原来只导目标在 FakeIP 网段和 IP 规则网段里的包（两个 nft 集合 `fakeip4`、`iprule4`，由 atomdns 维护），现在不再挑选，集合和 atomdns 维护集合的代码都删了。

依据是用户的部署：atom 在主路由后面，主路由只把 FakeIP 网段和要代理的网段转给 atom（单网卡用静态路由；双网卡的 LAN 接主路由的 WAN2，主路由把这些网段走 WAN2）。所以到达 atom 的转发流量都是要代理的，由 sing-box 决定出口：

| 目标 | sing-box 里怎么走 |
|---|---|
| FakeIP 网段（默认 `28.0.0.0/8`） | 有绑定走规则指定的组，没有走 Final；路由规则不参与（design.md 3.3） |
| 真实 IP | 按 IP、端口、网络规则（design.md 4.6）；没命中走 Final |

| 协议 | 去向 |
|---|---|
| TCP | redirect → `redirect-in` 7892 |
| UDP | TProxy → `tproxy-in` 7893 |
| 其他（ping 等） | 单网卡：不处理；双网卡：转发并 NAT 到 WAN（3.2） |

- 只看从局域网网卡进来的包（`iifname`）。目标是本机地址的包一律放过（`fib daddr type local return`），所以客户端问 VM 的 DNS（53）、管理接口和面板不会被导进 sing-box。广播和组播（DHCP、mDNS、SSDP 之类）也放过。
- **sing-box 自己发出的流量不会被导回来**：nft 只挂在 `prerouting` 上，本机进程（sing-box 的出站、atomdns 的上游查询）发出的包走 `output`，不经过这两条链。所以不需要给 sing-box 设 `routing_mark` 再在 nft 里排除。代价是本机进程访问 FakeIP 不会被处理（本机不需要：atomdns 的国外 DNS 走 7890，见 design.md 7.1）。
- IPv6：v0 不做 IPv6 FakeIP；规则只匹配 IPv4（`meta nfproto ipv4`），IPv6 的包照常转发或丢弃，IPv6 的 IP 规则只对 7890 入站有效。

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
# atom: send TCP and UDP from the LAN to sing-box.
table inet atom
delete table inet atom
table inet atom {
	# TCP: redirect to sing-box redirect-in (7892).
	chain prerouting_tcp {
		type nat hook prerouting priority dstnat; policy accept;
		iifname != { @LAN@ } return
		fib daddr type { local, broadcast, multicast } return
		meta nfproto ipv4 meta l4proto tcp redirect to :7892
	}

	# UDP: TProxy to sing-box tproxy-in (7893); the mark sends it to table 233 (local).
	chain prerouting_udp {
		type filter hook prerouting priority mangle; policy accept;
		iifname != { @LAN@ } return
		fib daddr type { local, broadcast, multicast } return
		ct direction reply return
		meta nfproto ipv4 meta l4proto udp tproxy ip to :7893 meta mark set 0x233 accept
	}
}
```

- 开头的 `table inet atom` + `delete table inet atom` 让文件可以重复加载：每次都整表替换，不会叠出重复规则。表里没有集合，加载完就生效，不用等 atomdns。
- 链名不能用 `redirect`、`tproxy`（nft 的关键字），所以叫 `prerouting_tcp` / `prerouting_udp`。
- `meta nfproto ipv4`：`inet` 表也看 IPv6，sing-box 的 redirect、TProxy 入站只监听 IPv4。
- `ct direction reply return`（UDP，codex 复审后加）：别处（比如双网卡时 WAN 那边）发起、经过这台机器的 UDP 流，LAN 里回过来的包不导进 sing-box，照常转发。TCP 的 redirect 在 nat 链里，只看新连接的第一个包，不受影响。

### 3.2 只有双网卡网关要的：`/etc/atom/atom-gateway.nft`

```nft
#!/usr/sbin/nft -f
# atom, dual-NIC gateway only: what is neither TCP nor UDP from the LAN is forwarded and NATed out of the WAN.
table ip atom_nat
delete table ip atom_nat
table ip atom_nat {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		iifname { @LAN@ } oifname "@WAN@" masquerade
	}
}
```

`@WAN@` 是外网网卡名（不带引号，文件里已经有）。同时要开转发：`net.ipv4.ip_forward = 1`（安装器由 `atom-dns.service` 的 `ExecStartPre` 设置）。TCP、UDP 都进了 sing-box，这张表现在只给非 TCP、UDP 的包（ping 等）用；安装器写的版本另有一条：WAN 网卡上丢掉发往面板端口的 TCP（design.md 9.2）。

这份文件不带防火墙：`forward` 链默认放行。VM 本身就是网关时，WAN 侧进来的连接要不要拦，由用户现有的防火墙决定；安装器只提示，不替用户加。**[建议]**

### 3.3 策略路由（两种形态都要）

```sh
ip rule add fwmark 0x233 lookup 233 priority 9233
ip route replace local 0.0.0.0/0 dev lo table 233
```

`ip rule` 重复添加会叠出多条，安装器先 `ip rule show | grep -q 'fwmark 0x233 lookup 233'` 判断。

单网卡旁路由**不需要** `ip_forward`：redirect 和 TProxy 都把包交给本机的 socket，不经过转发。联调时单网卡形态是在 `ip_forward=0` 下测通的。

## 4. 服务的顺序 **[定]**

2026-11-12 之前，规则文件里的集合是空的，由 atomdns 在启动和每次应用 sing-box 后用 `nft -f` 填（`singbox.nft_table`、`status.nft.sync`）。改成全部导入后没有集合了，atomdns 不再碰 nft，`atom_rules.yaml` 的 `nft_table`、`nft` 和 `GET status` 的 `nft` 都删了。

安装器的写法仍是 atomdns 的单元里先加载规则文件和策略路由：

```ini
ExecStartPre=/usr/sbin/nft -f /opt/atom/etc/atom.nft
# 网关形态再加两行：
ExecStartPre=/usr/sbin/nft -f /opt/atom/etc/atom-gateway.nft
ExecStartPre=/usr/sbin/sysctl -w net.ipv4.ip_forward=1
ExecStartPre=/opt/atom/lib/policy-route.sh
```

手动重新加载规则文件后不用再做别的。

## 5. 单网卡旁路由

```text
客户端 ──默认网关──> 主路由 ──静态路由 FakeIP 网段、要代理的网段──> VM（atomdns + sing-box）
   ^                                                              │
   └────────────────────── VM 直接回包（同一网段）─────────────────┘
```

- 客户端的 DNS 指向 VM（DHCP 下发）。
- 主路由加静态路由：**FakeIP 网段和要代理的网段**（IP 规则用到的网段）都指向 VM。只加 FakeIP 网段的话，IP 规则只对 7890 的流量有效。转到 VM 的 TCP、UDP 全部进 sing-box，没命中 IP、端口、网络规则的真实 IP 走 Final，所以不要把不想代理的网段转过来。IP 规则改了，主路由的静态路由也要跟着改。**[建议]**
- `@LAN@` 是 VM 唯一的网卡。
- **没有实测的风险**：去程经过主路由、回程由 VM 直接发给客户端，主路由只看到一半的连接。主路由的转发链如果有"丢弃 invalid 状态"的规则（RouterOS 的默认防火墙就有），可能把后续的包丢掉。处理办法二选一：在主路由的转发链里，对目标是 FakeIP 网段和 IP 规则网段的包，在 drop invalid 之前放行；或者用 DHCP option 121（无类别静态路由）把这些网段的路由直接下发给客户端，客户端就直接发给 VM，不经过主路由。联调是用 netns 模拟的（第 7 节），客户端上直接加了到 VM 的路由，相当于第二种办法，没有经过真实的主路由。

## 6. 双网卡网关

```text
客户端 ──> 主路由 ──FakeIP 网段、要代理的网段走 WAN2──> VM 的 LAN 网卡 ──┬─ TCP / UDP → sing-box → 节点（或 direct）
                                                                       └─ 其他（ping）→ 转发 + NAT → WAN 网卡 → 上级
```

- 用户的接法（2026-11-12）：VM 的 LAN 接主路由的 WAN2，主路由把 FakeIP 网段和要代理的网段走 WAN2。所以到达 VM 的转发流量都是要代理的，安装器的警告写的是"主路由需要把 FakeIP 网段和要代理的网段转到这台机器"（原来是"LAN 的全部流量都会经过这台机器"）。
- 规则：3.1 + 3.2 + 3.3，`ip_forward = 1`。客户端不在 LAN 网段时的回程路由（`policy-route.sh` 里的 client routes）和面板只对 LAN 开放的规则见 design.md 9.2，不变。
- 和 xray 那台机器（10.0.2.1，只读参考过）比：现在一样是把 LAN 进来的所有 TCP/UDP 都导进 sing-box（只放过目标是本机的包）；xray 另外在 `output` 链按目标打标记把本机流量也导回去，并用 mark `0xff` 排除 sing-box 自己的流量，atom 不处理本机流量，所以不需要。xray 的 TProxy 标记是 `0x1`、路由表 100，atom 换成 `0x233`、233，避免和已有配置撞上。

## 7. 联调实测（2026-11-09，mosdns-rust，临时环境）

环境：

- 网关：mosdns-rust 的根 namespace，跑 atomdns（调试版）和补丁版 sing-box（`e25694ea`）。两个都是临时的 systemd 单元（`atom-e2e.service`、`atom-sing-box.service`，单元文件在 `/etc/systemd/system/`，二进制在 `/root/atom-step4/bin/`），没有替换系统里的任何程序。atomdns 的单元带 `ExecStartPre=net-up.sh`（加载规则和策略路由）。
- 节点：本机另一个 sing-box（ss 20001 / vless 20002 / socks 20003），看它的日志判断连接走了哪个节点；出口是测试机自己直连，翻不了墙，所以只验证分流，不验证访问效果。
- 客户端：两个 network namespace，各用一对 veth 接到网关。
  - `cli1`（单网卡旁路由）：只有到 `28.0.0.0/8` 和各个 IP 规则地址的路由指向 VM（代替主路由的静态路由），没有默认路由；VM 上 `ip_forward=0`。
  - `cli2`（双网卡网关）：默认路由指向 VM，VM 的 LAN 是这对 veth，WAN 是 `eth0`，`ip_forward=1` 加 NAT。
  - 注意：netns 里的程序解析域名走的是宿主的 systemd-resolved，不是测试网关。脚本里一律先 `dig @网关` 再 `curl --resolve`。
- 脚本和输出在共享笔记目录 `step4-e2e/`（`run.sh` 到 `run4.sh`）。

结果都符合预期，详见 design.md 第 11 节第 4 条。单网卡和网关两种形态都实测过；第 5 节的主路由风险没有实测条件。这是按集合挑选目标时的实测；改成全部导入后的实测见第 8 节。

## 8. 已知限制

- **转过来的网段和本机自己的出站**（codex 复审指出，记为限制）：主路由把网段 R 转给这台机器。如果 sing-box 的 direct 出站（直连规则、没被代理的 IP 规则）、代理节点的地址或 DNS 上游落在 R 里，本机发往 R 的包走默认路由到主路由，又被主路由按同一条静态路由送回来，转不出去。原来按集合挑选时，IP 规则网段也有同样的问题；全部导入只是让"转过来的网段"不再需要同时写成 IP 规则。处理办法：主路由的这条路由排除来源是这台机器的包（按来源的策略路由），或者别让直连目标、节点和上游落在转过来的网段里。安装器和 atom 不处理主路由。

## 9. 全部导入的实测（2026-11-12，测试机 `atom`）

见共享笔记 `rules-nft.md` 和 design.md 第 11 节（结果写在完成后）。

## 10. 卸载

```sh
nft delete table inet atom
nft delete table ip atom_nat 2>/dev/null
ip rule del fwmark 0x233 lookup 233 priority 9233
ip route flush table 233
```
