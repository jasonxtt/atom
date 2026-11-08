# sing-box FakeIP 出口绑定补丁

AtomDNS 决定域名走哪个出口，把结果放在发给 sing-box FakeIP DNS 的查询里。sing-box 记住"域名 → 出口"，连接回来时直接用这个出口，不再匹配 `route.rules`。

协议与 DefaWong 在 jasonxtt/atomdns PR15 中的契约（`docs/rust/contracts/native-singbox-integration.md` 第 3、4、6、7 节）保持一致，sing-box 侧代码是在上游 v1.14.4 上独立实现的。

## 代码位置

- fork 目录：`sing-box/`（上游 `SagerNet/sing-box` 的 clone）
- 分支：`atom/fakeip-outbound`，基于 tag `v1.14.4`（`c005f353`）
- 改动：7 个文件，+196/-10，另有 3 个测试文件

| 文件 | 改动 |
|---|---|
| `adapter/inbound.go` | `InboundContext` 增加 `FakeIPOutbound` |
| `adapter/fakeip.go` | `FakeIPStore` 增加 `StoreOutbound`/`LookupOutbound`，`FakeIPStorage` 增加 `FakeIPStoreOutbound`/`FakeIPLoadOutbound` |
| `dns/transport/fakeip/fakeip.go` | 解析 EDNS0 65001，在分配或复用地址前更新绑定 |
| `dns/transport/fakeip/store.go` | 绑定读写转发到存储 |
| `dns/transport/fakeip/memory.go` | 内存存储增加 `outboundCache`，`FakeIPReset` 一并清空 |
| `experimental/cachefile/fakeip.go` | 新 bucket `fakeip_outbound`，`FakeIPReset` 一并删除 |
| `route/route.go` | 还原 FakeIP 时查绑定；有绑定时跳过 `route.rules`，TCP/UDP 严格按标签选出口，PreMatch 交回常规路径；`resolve` 动作解析出 FakeIP 时按域名查绑定（`resolvedToFakeIP`，见下文"代理入站"） |

上游 v1.14.4 已经对 FakeIP transport 关闭了 DNS client 缓存（`dns/router.go` 中 `isFakeIP` 时 `DisableCache = true`），所以不需要 PR15 契约里对 `dns/client.go` 的改动。

## 协议：EDNS0 Option 65001

通过 OPT RR 的 `EDNS0_LOCAL` 携带，载荷为 `[version=1, op, outbound...]`：

| op | 含义 |
|---|---|
| `1` SET | 后续字节是出口标签（UTF-8，去除首尾空白） |
| `2` CLEAR | 清除该域名的绑定 |

没有该选项，或长度不足、版本或 op 未知时，视为没有指令，已有绑定保持不变。所以清除绑定必须显式发 CLEAR。

## 行为

- 绑定按域名保存，同一域名的 A 和 AAAA 共用一个绑定。
- 有绑定的 FakeIP 连接不匹配 `route.rules`，按标签查出口。标签可以是 selector 或 urltest，最终落到组内当前选中的节点。
- 标签不存在时报错 `outbound not found: <tag>`，连接被阻断，不会回退到默认出口。
- 没有绑定的域名照常走 `route.rules` 和 `route.final`。
- 已建立的连接不会因为绑定变化而迁移。
- 绑定写失败时只记 warning，DNS 仍正常应答。
- 开启 `experimental.cache_file` 的 `store_fakeip` 后，绑定可以跨重启保留。

## 代理入站（mixed 7890）

浏览器经 mixed 入站发来的是域名，不经过 FakeIP 地址，补丁的第一部分管不到。atom 生成的路由规则先对 mixed 入站做 `resolve`（`server` 指向本机 AtomDNS，`disable_cache: true`），补丁在 `matchRule` 里 `resolve` 成功之后检查一次：

- 解析出的地址全部属于 FakeIP 网段：说明 AtomDNS 决定代理，并且已经通过 FakeIP DNS 写好了绑定（AtomDNS 先问 sing-box 再回应答，没有时序问题）。补丁丢掉这些地址，目标保留域名，由代理服务器远端解析；按地址分配时的域名查绑定，有绑定走对应的组，没有走 `route.final`。之后的路由规则不再匹配。
- 有真实地址：不处理，交给后面的路由规则（atom 生成的是"IP 规则 → 组，其余域名 → direct"）。
- 空应答或 NXDOMAIN：上游 `resolve` 本来就报错（`empty result`），连接被拒，等于屏蔽。

直接发来 IP 的连接不做 `resolve`，只匹配 IP 规则，没命中走 `route.final`。

## 已验证

在本机（macOS）运行 `go test`：

- `dns/transport/fakeip/outbound_test.go`：EDNS 解析的各种情况；SET 复用已有地址；A/AAAA 共用绑定；无指令不改变绑定；CLEAR 清除；Reset 清空。
- `route/fakeip_outbound_test.go`：进程内启动真实的 sing-box，路由规则把 socks 入站全部 reject。
  - 未绑定时被拒；SET 后 TCP 和 UDP 都能通过，UDP 回包源地址是 FakeIP；CLEAR 后又被拒；绑定到不存在的出口时阻断。
  - 开启 cache file 后重启实例，不重新查 DNS，绑定仍然生效；CLEAR 后重启，仍保持清除状态。
- `route/fakeip_mixed_test.go`：mixed 入站 + `resolve`，DNS 是一个模拟 AtomDNS 的测试服务（代理的域名转问 sing-box 的 FakeIP DNS 并带 SET/CLEAR，直连的域名给真实地址，屏蔽的给空应答），用连接 tracker 记录实际出口：代理 → 绑定的组、域名保留；没有绑定的 FakeIP → `route.final`；真实地址 → direct；空应答 → 连接失败；直接发 IP → `route.final`。
- 反向验证：去掉 `route/route.go` 的改动后，上面三个端到端测试会失败（mixed 的测试单独做过：去掉 `resolvedToFakeIP` 后在"代理"那一步失败）。
- `route/...`、`dns/...`、`experimental/cachefile/...`、`adapter/...`、`option/...` 原有测试全部通过，`go vet` 无告警。

## 未验证

- 没有在 Linux 上配合 TProxy 或 TUN 做实机测试。
- PreMatch 只在 TUN 下生效。有绑定时补丁把连接交回常规路径，这条路径没有在 TUN 下测试。
- 没有和 AtomDNS 联调。AtomDNS 侧的 EDNS 编码还没有实现。

## 已知限制

- 绑定只在收到 SET 或 CLEAR 时变化。FakeIP 地址池循环复用时，旧域名的绑定不会被清理：在内存里，或在 cache file 的 `fakeip_outbound` bucket 里。条目数受"带过绑定的不同域名数"限制。
- sing-box 不校验标签格式，受信边界在 AtomDNS 侧：AtomDNS 要先剥离客户端自带的 65001 选项，再写入自己的指令。

## 跟进上游

```bash
cd sing-box
git fetch origin --tags
git rebase <新 tag>
go test ./dns/transport/fakeip/ ./route/ -run 'FakeIP|Outbound' -count=1
```

冲突面集中在 `route/route.go` 的 `routeConnection`、`routePacketConnection`、`PreMatch`、`prepareMatchMetadata`、`matchRule` 这五处。
