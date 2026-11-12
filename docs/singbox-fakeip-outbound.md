# sing-box FakeIP 出口绑定补丁

AtomDNS 决定域名走哪个出口，把结果放在发给 sing-box FakeIP DNS 的查询里。sing-box 记住"域名 → 出口"，连接回来时直接用这个出口，不再匹配 `route.rules`；没有绑定的 FakeIP 连接直接走 `route.final`，同样不匹配 `route.rules`（2026-11-12 改，见下文"行为"）。

协议与 DefaWong 在 jasonxtt/atomdns PR15 中的契约（`docs/rust/contracts/native-singbox-integration.md` 第 3、4、6、7 节）保持一致，sing-box 侧代码是在上游 v1.14.4 上独立实现的。

## 代码位置

- fork 目录：`sing-box/`（上游 `SagerNet/sing-box` 的 clone）
- 分支：`atom/fakeip-outbound`，基于 tag `v1.14.4`（`c005f353`）
- 改动：9 个文件，另有 6 个测试文件

| 文件 | 改动 |
|---|---|
| `adapter/inbound.go` | `InboundContext` 增加 `FakeIPOutbound` |
| `adapter/fakeip.go` | `FakeIPStore` 增加 `StoreOutbound`/`LookupOutbound`，`FakeIPStorage` 增加 `FakeIPStoreOutbound`/`FakeIPLoadOutbound` |
| `dns/transport/fakeip/fakeip.go` | 解析 EDNS0 65001，在分配或复用地址前更新绑定；绑定保存失败时这次查询报错；写绑定和分配地址在一把锁里做完（`bindAccess`） |
| `dns/transport/fakeip/store.go` | 绑定读写转发到存储；地址被回收给别的域名时删掉旧域名的绑定（`dropReclaimedOutbound`） |
| `dns/transport/fakeip/memory.go` | 内存存储增加 `outboundCache`，`FakeIPReset` 一并清空 |
| `experimental/cachefile/fakeip.go` | 新 bucket `fakeip_outbound`，`FakeIPReset` 一并删除 |
| `cmd/sing-box/cmd_run.go` | 重载循环提成 `runLoop`；新配置启动失败时用上一份配置重建实例，进程不退出（见下文） |
| `experimental/clashapi/cache.go` | `/cache/fakeip/flush` 经 FakeIP store 的 `Reset()` 清（拿同一把锁），不再绕过它直接清 cache file；store 不用 cache file 时照上游另清 cache file（2026-11-12，codex 第 3 轮） |
| `route/route.go` | 还原 FakeIP 时查绑定；FakeIP 连接一律跳过 `route.rules`：有绑定时 TCP/UDP 严格按标签选出口，没有绑定走 `route.final`，PreMatch 交回常规路径；`resolve` 动作解析出 FakeIP 时按域名查绑定（`resolvedToFakeIP`，见下文"代理入站"） |

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
- FakeIP 连接（redirect、TProxy 的 TCP 和 UDP，以及 PreMatch）不匹配 `route.rules`：目标是 FakeIP，说明 AtomDNS 判定这个域名走代理，出口要么是规则指定的组（绑定），要么是 Final。
  - 有绑定：按标签查出口。标签可以是 selector 或 urltest，最终落到组内当前选中的节点。标签不存在时报错 `outbound not found: <tag>`，连接被阻断，不会回退到默认出口。
  - 没有绑定（CLEAR 过，或从没带过指令）：直接走 `route.final`。原来（到 `e7b78640`）没有绑定的连接照常走 `route.rules` 和 `route.final`，一条能匹配它的路由规则（比如端口规则）会把它截走；2026-11-12 用户定为不参与（atom design.md 3.3）。mixed 入站的 `resolvedToFakeIP` 本来就是这样，三个入口现在一致。
  - 所以 atom 生成的 IP、端口、网络规则只对真实 IP 的连接生效。
- 已建立的连接不会因为绑定变化而迁移。
- 规则变化后，AtomDNS 的绑定账本会主动给有变化的域名发 SET 或 CLEAR（不用等客户端重新查询，见 [design.md](design.md) 4.5.1）；sing-box 这边不用改。
- 绑定写失败时（比如 cache file 写不进去），这次查询不返回应答（报错，客户端看到的是 SERVFAIL 或超时），已有绑定保持不变。原来是只记 warning、照常应答，但 AtomDNS 的绑定账本把正常应答当作"绑定已生效"，之后不会再发；改成报错后，账本保留旧值，下次重算再发（atom 第 5 步 codex 第 2 轮复审后改）。不带指令的查询不受影响。
- 开启 `experimental.cache_file` 的 `store_fakeip` 后，绑定可以跨重启保留。
- 地址回收（2026-11-12）：FakeIP 地址池循环复用、把某个地址分给新域名时，删掉原来占着这个地址的旧域名的绑定（内存和 cache file 都一样）。只删映射到这个地址的旧域名；旧域名还有另一族的地址（A 和 AAAA 共用一个绑定）时不删。为了不误删正在写的绑定，FakeIP DNS 处理一次查询时，写绑定和分配地址在同一把锁里做完：否则另一个查询可能在"旧域名刚写好 SET"和"旧域名重新分配地址"之间回收它的地址、删掉刚写的绑定，旧域名就会拿着地址却没有绑定（走 Final 而不是规则指定的组）。代价是 FakeIP DNS 的查询串行处理。清空（`Store.Reset()`，包括 Clash API 的 `/cache/fakeip/flush`）也拿这把锁，不会落在一次查询的"写绑定"和"分配地址"之间。原来 flush 直接清 cache file；现在没开 `store_fakeip` 时 flush 也会清内存里的映射和绑定（atom 一直开着）。旧域名的客户端下一次查询时，AtomDNS 照常带指令（代理规则的查询不经过缓存），绑定随新地址重建；AtomDNS 的绑定账本不知道这次删除，账本里的旧值最多让下一次重算少发一次相同的指令，不影响新查询。

## 代理入站（mixed 7890）

浏览器经 mixed 入站发来的是域名，不经过 FakeIP 地址，补丁的第一部分管不到。atom 生成的路由规则先对 mixed 入站做 `resolve`（`server` 指向本机 AtomDNS，`disable_cache: true`），补丁在 `matchRule` 里 `resolve` 成功之后检查一次：

- 解析出的地址全部属于 FakeIP 网段：说明 AtomDNS 决定代理，并且已经通过 FakeIP DNS 写好了绑定（AtomDNS 先问 sing-box 再回应答，没有时序问题）。补丁丢掉这些地址，目标保留域名，由代理服务器远端解析；**按连接请求的域名**查绑定（查不到再试一次小写），有绑定走对应的组，没有走 `route.final`。之后的路由规则不再匹配。
- 不用地址反查域名（codex 复审后改）：AtomDNS 从缓存里答的 FakeIP，在 sing-box 那边可能已经因为地址池循环分给了别的域名，或者映射已经丢了；反查会拿到别的域名的绑定，或者查不到。连接本来就带着目标域名，直接按它查，两种情况都不会出错：没绑定的域名就走 `route.final`，和 AtomDNS 的判断一致（代理规则的查询不经过 AtomDNS 缓存，每次都带 SET）。
- 有真实地址：不处理，交给后面的路由规则（atom 生成的是"IP、端口、网络规则 → 对应的组，其余域名 → direct"）。
- 空应答或 NXDOMAIN：上游 `resolve` 本来就报错（`empty result`），连接被拒，等于屏蔽。

直接发来 IP 的连接不做 `resolve`，只匹配 IP、端口、网络规则，没命中走 `route.final`。

## 重载失败时保留服务（`cmd/sing-box/cmd_run.go`）

上游的重载循环：收到 SIGHUP 先 `check`，通过后关掉旧实例，再按新配置创建；创建失败（端口被占用、出站初始化之类 `check` 发现不了的错误）就返回错误，进程退出。补丁把循环提成 `runLoop`，记住上一份成功运行过的配置：

- 新配置创建失败：记一条 `reload service: ..., restoring the previous configuration`，用上一份配置重建实例，继续等信号。重建也失败才退出。
- 重建期间收到的 SIGHUP 不会打断重建（codex 复审后改）：`create` 在重建时只监听终止信号，SIGHUP 留在运行循环的信号队列里，重建完成后再按它重载一次；SIGINT/SIGTERM 照常中止。
- 终止信号不会丢（codex 第二次复审后改）：运行循环的信号通道只有一个位置，排队的 SIGHUP 占住它以后，之后来的 SIGTERM 会被丢掉。所以 `create` 在构造实例**之前**就开始监听终止信号，并记下构造或启动期间是否收到过；收到过就返回 `errStopped`（即使启动本身没有因为 context 取消而失败，也会关掉实例），运行循环见到它就正常退出，不再执行排队的重载。
- 重读配置文件失败：保留当前配置重新启动。
- 第一次启动失败照旧退出（没有可退回的配置）。

atom 用新的 Clash API 密钥确认重载，旧实例不认它，所以确认失败后 atom 照常写回旧文件、再重载一次，补丁只是让中间不断服务。

## 已验证

在本机（macOS）运行 `go test`：

- `dns/transport/fakeip/outbound_test.go`：EDNS 解析的各种情况；SET 复用已有地址；A/AAAA 共用绑定；无指令不改变绑定；CLEAR 清除；Reset 清空。绑定保存失败时查询报错、旧绑定不变、不带指令的查询照常应答、存储恢复后指令生效（反向验证：改回只记 warning 后这个测试失败）。
- `route/fakeip_outbound_test.go`：进程内启动真实的 sing-box，路由规则把 socks 入站发往回显端口的连接走 direct（一条端口规则），其余 reject；`route.final` 是 `block`（2026-11-12 改）。
  - 真实地址按端口规则通过；未绑定的 FakeIP 即使端口规则能匹配也走 Final、被拦；SET direct 后 TCP 和 UDP 都能通过，UDP 回包源地址是 FakeIP；SET final 时端口规则也不参与；CLEAR 后回到 Final；绑定到不存在的出口时阻断。反向验证：把 `matchRule` 改回"有绑定才跳过"，"未绑定走 Final"这一步失败。
  - 开启 cache file 后重启实例，不重新查 DNS，绑定仍然生效；CLEAR 后重启，仍保持清除状态。
- `route/fakeip_mixed_test.go`：mixed 入站 + `resolve`，DNS 是一个模拟 AtomDNS 的测试服务（代理的域名转问 sing-box 的 FakeIP DNS 并带 SET/CLEAR，直连的域名给真实地址，屏蔽的给空应答），用连接 tracker 记录实际出口：代理 → 绑定的组、域名保留；没有绑定的 FakeIP → `route.final`；真实地址 → direct；空应答 → 连接失败；直接发 IP → `route.final`。
- `cmd/sing-box/cmd_run_test.go`：用假的实例驱动 `runLoop`：重载到启动失败的配置后重建旧实例、进程不退出，下一次重载正常；重建期间到达的 SIGHUP 不打断重建、重建后再处理（假实例按真实 `create` 的行为：监听 SIGHUP 的启动会被取消）；重建期间又来了终止信号时正常退出、不执行排队的重载；另有一个调用真实 `create` 的测试：运行循环的通道已被 SIGHUP 占满，在构造实例前给自己发 SIGTERM，`create` 返回 `errStopped`；重读失败时保留当前配置；第一次启动失败照旧返回错误。反向验证：去掉重建那一段、重建时也监听 SIGHUP、把信号监听挪回构造实例之后、或者不检查 `errStopped`，对应的测试失败。
- `experimental/clashapi/cache_test.go`（2026-11-12）：flush 经过 store 的 `Reset()`；store 用 cache file 时不另清，不用时另清；没有 FakeIP 服务器时只清 cache file；失败返回 500。反向验证：换回上游的 flush，测试失败。测试机 `atom` 上实测：flush 返回 204，之后 `api.openai.com` 重新分到地址、带 SET 重新绑定，连接走 AI 组。
- `dns/transport/fakeip/reclaim_test.go`（2026-11-12）：单地址的地址池（`/30`）里，新域名回收地址时删掉旧域名的绑定（内存和 cache file 两种存储），不带指令的回收也删；旧域名还有 AAAA 地址时保留；无关域名的绑定不动；旧域名带 SET 重新查询后绑定恢复。另一个测试让存储在写完 a 的 SET 后停一下，这时 b 来回收地址：结束后凡是占着地址的域名都必须有绑定。反向验证：去掉删除那一段，前两个测试失败；去掉 `bindAccess` 锁，最后一个测试失败。
- `route/fakeip_mixed_test.go` 加了"缓存里的 FakeIP 已经属于另一个有绑定的域名"：按请求的域名查，走 `route.final`；改回反查后这一步拿到别的域名的出口，测试失败（反向验证）。
- 反向验证：去掉 `route/route.go` 的改动后，上面三个端到端测试会失败（mixed 的测试单独做过：去掉 `resolvedToFakeIP` 后在"代理"那一步失败）。
- `route/...`、`dns/...`、`experimental/cachefile/...`、`adapter/...`、`option/...` 原有测试全部通过，`go vet` 无告警。

- 和 AtomDNS 联调（第 4 步，Linux，nft redirect + TProxy，见 [deploy-nft.md](deploy-nft.md) 第 7 节）：redirect 入站的 TCP、TProxy 入站的 UDP 都按绑定出去，UDP 回包源地址是 FakeIP；cache file 里的绑定在 sing-box 重启后仍然生效；重载失败时进程不退出。TProxy 写回 UDP 时要绑定"原目标地址:端口"，和本机在同一端口通配地址上、没设 `SO_REUSEADDR` 的监听冲突（atomdns 的 53，已在 atomdns 侧修，见 design.md 9.4）。

## 未验证

- 没有在 TUN 下测试。PreMatch 只在 TUN 下生效；有绑定时补丁把连接交回常规路径，这条路径没有在 TUN 下测试。

## 已知限制

- 启动期间的信号竞态（codex 第三轮复审指出，决定记为限制、不改代码）。两种情况：
  - 恢复旧配置期间，SIGHUP 先到、占住运行循环那个只有一个位置的信号通道，紧接着终止信号（SIGINT/SIGTERM）在启动快结束时到达 `create` 自己的监听通道。如果监听 goroutine 还没来得及记下"收到过终止信号"，`create` 就已经检查完并返回成功，这个终止信号就被漏掉了。
  - 普通重载启动期间连续收到 SIGHUP：`create` 的监听只处理收到的第一个信号；SIGHUP 先占住两个通道后，再来的终止信号被丢掉（或者留在监听通道里没人读）。
  
  影响：进程没有退出，又按排队的 SIGHUP 重载了一次。要信号几乎同时到达才会触发，概率很小。处理办法：再发一次终止信号（`systemctl stop` 超时后 systemd 也会改用 SIGKILL）。

- 改 FakeIP 网段的那次重载如果失败，旧映射找不回来：新实例启动时 `Store.Start()` 发现网段不同，会清空 cache file 里的 FakeIP 映射和绑定，之后新实例才因为别的原因（比如端口被占）启动失败；重建旧实例时映射已经没了，客户端手里的旧 FakeIP 会报 `missing fakeip record`，要等客户端重新查 DNS。不单独处理：就算换网段成功，旧映射一样会清空、旧 FakeIP 一样失效，改网段本身就是破坏性的操作。

- 绑定在收到 SET 或 CLEAR 时变化，地址被回收给别的域名时删除（见"行为"）。旧域名还占着另一族地址时绑定保留，atom 不开 IPv6 网段，所以实际不会出现。2026-11-12 之前回收不清理绑定，升级前留在 cache file 的 `fakeip_outbound` bucket 里、已经没有地址的旧绑定不会自动清掉，只在那个域名的地址再次被回收或收到 CLEAR 时消失；它们不影响路由（没有地址的域名不会有连接）。
- sing-box 不校验标签格式，受信边界在 AtomDNS 侧：AtomDNS 要先剥离客户端自带的 65001 选项，再写入自己的指令。

## 跟进上游

```bash
cd sing-box
git fetch origin --tags
git rebase <新 tag>
go test ./dns/transport/fakeip/ ./route/ -run 'FakeIP|Outbound|Reclaim' -count=1
```

冲突面集中在 `route/route.go` 的 `routeConnection`、`routePacketConnection`、`PreMatch`、`prepareMatchMetadata`、`matchRule` 这五处。
