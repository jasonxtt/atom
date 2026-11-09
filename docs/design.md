# atom 设计文档 v0

状态：v0 已审，第 12 节的问题已答复；第 2 步实现时新定的事项写回了第 3、4、8 节，第 3 步的写回了第 4.6、5、6、8 节，第 4 步的写回了第 4.6、6、7.3、9.4、11 节，第 5 步的写回了第 4.5、7.2、8、11 节。日期：2026-11-09。

标记说明：**[定]** 表示已经和用户确认；**[建议]** 是我的提议，等审阅；**[待定]** 表示还要讨论。

## 1. 目标

做一个给别人用的分流网关：一台 VM 同时跑 DNS 和代理。只有一个决策点，规则由控制面统一管理，用安装器一键部署。

- 由 DNS 决定域名走哪个出口，代理核心只负责执行，不再做第二次判断。**[定]**
- 用户只面对一套规则和一个界面。AtomDNS 原有的 WebUI 和大部分选项不再暴露给用户。**[定]**
- 后端先支持 sing-box，以后再加 mihomo。机场订阅用户用 mihomo 自带的 provider，控制面不做订阅。**[定]**
- 先把后端 API 规范定好，自带一个简单的 WebUI；更完善的 UI 可以交给别人开发。**[定]**

不在本期范围：订阅管理、多台机器部署、IPv6 FakeIP（见 9.3）。

## 2. 组件

```text
              ┌──────────────── atom（AtomDNS fork，一个 Rust 二进制）────────────────┐
客户端 DNS ──>│ DNS 53：规则表 → 原国内外判断 → 应答（真实 IP / FakeIP + EDNS 65001）  │
              │ 控制面：规则、节点、节点组、Final、设置；生成并下发 sing-box 配置       │
              │ 管理 API（/api/v1/...）+ 简易 WebUI                                    │
              └──────────────┬──────────────────────────────────┬──────────────────────┘
                     DNS + EDNS0 65001               写配置 + SIGHUP / Clash API
                             v                                  v
              ┌────────────── sing-box（fork，Go 进程）──────────────┐
FakeIP 流量 ─>│ FakeIP 还原域名 → 绑定的出口 / route.final → 节点     │
              └──────────────────────────────────────────────────────┘
```

| 组件 | 来源 | 职责 |
|---|---|---|
| atom | `atom/atomdns`：从 `jasonxtt/atomdns` main 分出 **[定]** | DNS 决策、规则表、控制面、管理 API、WebUI 托管 |
| sing-box | `atom/sing-box`：分支 `atom/fakeip-outbound`，基于 v1.14.4 **[定]** | FakeIP、出口绑定、节点协议、节点组选择和测速 |
| 安装器 | `atom/installer` **[建议]** | 安装两个程序和 systemd 服务，配置 nft、路由和 sysctl |

- 两个进程之间只通过 DNS 和 HTTP 通信，不共享内存，也不用 cgo。
- fork 和 AtomDNS 主线差得多没关系，好用优先。**[定]** 主线的修复按需合并过来，不承诺跟进。
- Rust 编译按 AtomDNS 的规矩放到远端机器 `mosdns-rust` 上做。sing-box 的 Go 编译量小，可以在本地做。**[建议]**

## 3. DNS 决策流程

### 3.1 查询路径

```text
1. 系统固定项        重写/hosts、本地 PTR、类型屏蔽（SOA/PTR/HTTPS）、DDNS   不可拖动
2. 用户规则表        有序，第一条命中就生效，可拖动                      见第 4 节
     代理 → 组 X     直接返回 FakeIP，EDNS SET X；不问国内外 DNS
     直连            问国内 DNS，返回真实 IP
     屏蔽            返回空应答或 NXDOMAIN
3. 原国内外判断      AtomDNS 现有的 compat/safe 流程、域名学习、geosite/geoip
     国外 → FakeIP，EDNS CLEAR，由 sing-box 的 route.final（Final 组）处理
     国内 → 真实 IP，直连
```

用户规则命中"代理"时，AtomDNS 不用再问国内 DNS，不用判断 geoip，也不用问国外 DNS。域名由代理服务器在远端解析，既省掉了查询，也不会把域名泄露给国内 DNS。**[定]**

### 3.2 和 AtomDNS 现有流程的对应

现有主流程是 `config_custom.yaml` 里的 `sequence_6666`，共 11 步。fork 里这样处理：

| 现有步骤 | fork 中的处理 |
|---|---|
| 1 重写、2 类型屏蔽 | 保留，成为"系统固定项" |
| 3 `unified_matcher1` 名单打标 | 用户维护的名单（黑名单、白名单、灰名单、`cuscn`、`cusnocn`、`realiplist`、`cnfakeipfilter`）改由规则表表达；`geosite_cn` / `geosite_no_cn` 和学习结果留给第 3 层的原判断 **[建议]** |
| 4/6 专属分流组 | 删除 **[建议]** |
| 5 黑名单、广告 | 广告变成规则表里的普通规则，可以拖动（见 4.4）；第 2 步删掉了固定的广告屏蔽步骤和 `block_ads` 开关。黑名单屏蔽暂时保留，挪到规则表后面，第 6 步并入规则表 **[定]** |
| 7 DDNS | 保留现有的 DDNS 名单和流程（不缓存，`ddns_foreign` 开关保留），名单在控制面编辑。**[定]** 放在系统固定项里、排在规则表之前。**[建议]** |
| 8 指定客户端直连 | 变成规则条件"来源客户端"（见 4.2） |
| 9 全局缓存 | 保留，排在规则表后面；规则变化时清空（见 4.5） |
| 10/11 名单内、名单外 | 保留，成为第 3 层的原判断 |

现有的 `domain_mapper` 是把所有命中名单的 mark 合并起来，不是"第一条命中"。规则表需要新写一个匹配插件，可以复用 `matcher-core` 里的 trie、keyword 和 regex 实现。**[建议]**

### 3.3 EDNS0 65001

协议见 [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md)。

- 只在发往本机 sing-box FakeIP 入口的查询上附加；其他上游不带这个选项，避免泄露到公网。
- 先剥掉客户端自带的 65001 选项，再写入自己的指令。
- 规则命中"代理 → 组 X"时发 SET X；原判断的结果是国外时发 CLEAR，由 `route.final` 处理。所以大部分域名不会在 sing-box 里留绑定记录。**[定]**
- 实现（第 1 步）：在 fork 的 `rust/native-host/src/edns_directive.rs`，挂在 `ForwardCatalog` 发往上游的出口上，所有上游查询都经过这里。发往 `nocnfake` 组的查询：去掉客户端带的 65001，加上 CLEAR（`[1,2]`）；客户端没有 OPT 时补一个 OPT（UDP 大小 1232）。发往其他组的查询：没有 65001 时原样发出；带了就去掉，其他 option（ECS、cookie 等）保留。剥 65001 只按 OPT 的结构逐个遍历选项，不依赖 ECS 等选项的语义校验（比如重复 ECS 时照样剥）；附加记录连结构都解析不了的查询，对所有上游都拒绝，不原样发出。`nocnfake` 的查询还要通过完整的 OPT 校验，过不了就报错，不发给 sing-box。改写过的查询一律用展开后的域名重建问题段，因为原包里的压缩指针可能指向后面会移动的选项字节。
- 注意：FakeIP 应答会进全局缓存（`cache_all`），命中缓存时不再问 sing-box，也就不会重发 CLEAR/SET。第 1 步只有 CLEAR，不受影响；第 2 步发 SET 以后，规则变化要按 4.5 清缓存。
- SET 的实现（第 2 步）：规则表插件命中"代理 → 组 X"时，把 X 写进查询状态（`ExecutionState::outbound`，在 `sequence-core`）；`ForwardCatalog` 发往上游时把它交给 `edns_directive.rs`，发往 `nocnfake` 的查询写 SET X，没有 X 时写 CLEAR，其他上游不带。规则表排在 `cache_all` 前面，命中代理规则的查询不经过全局缓存，每次都会把 SET 发给 sing-box；规则每次变化都清空所有缓存（4.5 第 1 条），所以删掉代理规则后，`cache_all` 里不会留着旧的 FakeIP 挡住 CLEAR。**[定]**
- 附加记录的 owner 名（第 2 步修）：先按问题段的规则校验展开后的名字（压缩指针不能越界、不能自指、不能成环），再接受这条记录；以前只是跳过 owner，这类 TYPE=OPT 的记录会被原样发给普通上游。

## 4. 规则表 **[定]**

### 4.1 模型

```text
Rule {
  id          不可变 ID
  enabled     是否启用
  remark      备注（显示用）
  match       匹配条件，见 4.2
  action      proxy(group_id) | direct | block
}
```

规则表是一个有序数组，从上往下，第一条命中的生效，没命中就进入第 3 层原判断。

### 4.2 匹配条件 **[定]**

| 类型 | 说明 |
|---|---|
| `domain` / `suffix` / `keyword` / `regex` | 内联的域名条目 |
| `ruleset` | 引用一个规则集：内置 geosite 分类、远程列表 URL，或本地列表 |
| `client` | 来源客户端 IP 或网段，可以和域名条件组合 |
| `ip_cidr` | 目标 IP 网段，用于 Telegram 这类直接按 IP 连接的流量，见 4.6 |

一条规则内部是"或"还是"与"：**[定]** 同一类条件之间是"或"，不同类之间是"与"。比如"suffix google.com 或 youtube.com，并且客户端是 192.168.1.50"。

"类"的划分（第 2 步实现时定下）：`domain`、`suffix`、`keyword`、`regex`、`ruleset` 都是域名条件，算同一类；`client` 是第二类；`ip_cidr` 由 sing-box 匹配（4.6）。所以"domain a.com，ruleset 广告"表示 a.com 或广告域名。只有 `client` 条件的规则匹配这个客户端的所有域名。规则至少要有一个条件。

带 `client` 条件的规则，动作只能是直连或屏蔽，不能是代理。**[定]** 原因：sing-box 只按域名绑定出站组，不区分客户端。比如"客户端 A 的 foo.com → 组 X"：A 一查就发 SET X，于是别的客户端拿到 foo.com 的 FakeIP 后也走 X。别的客户端从缓存拿到 FakeIP 时不发 CLEAR；不走缓存时，A 的 SET 和别人的 CLEAR 会来回覆盖。所以代理规则对所有客户端都一样，客户端条件只用来决定这个客户端能不能走代理（直连或屏蔽）。sing-box 不增加按客户端分流的功能。API 新建或修改这种规则时返回 400。

### 4.3 拖动排序

- 每条规则有显式的顺序。API 提供移动操作：`{id, before_id | after_id, version}`。
- 规则表整体带一个版本号，写入时比较版本，防止两个人同时编辑时互相覆盖。
- 修改后重新编译匹配器并原子替换，不用重启，DNS 不中断。

### 4.4 广告和黑名单

广告（AdGuard 在线规则）和黑名单也是规则表里的普通规则，动作是"屏蔽"，可以拖动。用户要放行某个误杀的域名，把一条"直连"规则拖到它前面就行。

新安装时的默认规则表 **[定]**：

1. 局域网和私有域名 → 直连（后缀 `lan`、`local`、`localhost`、`localdomain`、`home.arpa`、`internal`）
2. 广告 → 屏蔽（`ruleset: adguard`，按 AdGuard 规则自己的屏蔽/放行判断）

其余全部交给原判断。`atom_rules.json` 不存在时使用默认规则表，第一次修改时才写文件（`atomdns check` 不会写）。

黑名单、白名单、灰名单、`cuscn`、`cusnocn` 这些自带名单暂时留在原判断里，第 6 步换 WebUI 时再并入规则表。**[定]**

### 4.5 规则变化后的生效

sing-box 的绑定只在收到 DNS 查询时更新，而客户端会缓存 DNS 应答，所以规则改动天然会有滞后。处理办法：

1. 清空 AtomDNS 的各级缓存。v0 直接全清，以后再细化为按受影响的域名清。**[定]** 第 2 步已实现：`/api/v1/atom/` 下每次成功的修改（规则、规则集）都清空配置里的所有缓存。
2. **绑定账本**：AtomDNS 在内存里记录发给 sing-box 的每个域名最后一次被确认的指令，用 LRU 限制大小。规则变化后，用新规则重算这些域名，把有变化的 SET/CLEAR 直接用 DNS 查询发给 sing-box，不需要新接口。这样"组 X 改成组 Y"可以立即生效，客户端不用重新查询。**[定]** 第 5 步实现，细节见下面的 4.5.1。
3. 直连和代理互换的情况，客户端手里拿的是真实 IP，只能等它的 DNS 缓存过期。直连应答的 TTL 设上限 `direct_ttl_max`，默认 **300 秒**，可以通过 `PUT settings` 修改。**[定]**
4. **FakeIP 应答的 TTL** 设上限 `fakeip_ttl_max`，默认 **1 秒**，和生产环境的用法一致；也可以通过 `PUT settings` 修改。sing-box 给的是 600 秒（`C.DefaultDNSTTL`），太长了。**[定]** 有了 1 秒的 TTL，客户端很快会重新查询，新规则随之生效；账本主要用来照顾不遵守短 TTL 的客户端，比如 Java、浏览器自带的 DNS 缓存。

#### 4.5.1 实现（第 5 步）**[定]**

- **账本**（`rust/native-host/src/atom_ledger.rs`）：键是查询问题段里的原名字（wire 形式，保留大小写，sing-box 按原样保存绑定），值是 SET 某个标签或 CLEAR。容量固定 16384 条，不对用户开放，最久没有记录的先淘汰。账本是进程级的，AtomDNS 重新加载配置时不丢，进程重启后为空。
- **记录**：`ForwardCatalog` 发往 `nocnfake` 的查询拿到 sing-box 的应答后，按带的指令记一条（SET 组，或者 CLEAR）。账本自己发的指令被确认后也更新。SET 和 CLEAR 都记：原判断判成国外的域名也在账本里，用户给它加了规则以后，客户端手里的 FakeIP 立即跟着走。
- **重算**：对账本里的每个域名，用新规则表重新判断。客户端按"不属于任何客户端条件"处理，带 `client` 条件的规则一律不命中：这类规则只能是直连或屏蔽，命中它的查询不会发给 sing-box，所以 sing-box 里的全局绑定反映的是其他客户端得到的结论（4.2）。内置规则集的 fast_flags 按 mapper 现场算，和 `rules/test` 一样。新结论对应的指令：
  - 代理 → 组 Y：SET Y；
  - 直连：SET `direct`；屏蔽：SET `block`（生成的配置里都有这两个出站），客户端手里的 FakeIP 立即直连或被拒，直连时由 sing-box 用 resolver 解析域名；
  - 没有规则命中（交给原判断，没法离线算）：CLEAR，交给 Final，和"国外"的结论一致。
  
  和账本里的旧值一样就不发。
- **发送**：用 UDP 直接向 `singbox.dns_in`（127.0.0.1:6666）发 A 查询，带 65001 指令，格式和客户端查询经过 `edns_directive.rs` 后的一样。每次最多 32 个同时在途，单次等 1 秒，没回应就再试一次。
- **时机**：同步做。修改落盘、清空缓存之后，接口返回之前；进程里一把锁串行，后开始的重算一定用最新的规则表。接口返回时，sing-box 的绑定已经改好。
- **失败**：修改照样生效，接口照样返回成功，因为规则已经保存了。sing-box 没确认的域名，账本保留旧值，下次重算再试；客户端下一次查询也会带上新指令（代理规则的查询不经过缓存）。失败记日志，`GET status` 的 `bindings` 给出 `{size, capacity, last: {at, trigger, changed, failed, error?}}`。
- **各种改动之后**：
  - 规则的增、删、改、拖动、启用、停用，以及规则集的新建、修改、删除、`refresh`：都重算；
  - 带 `confirm=true` 删除节点组：引用它的规则改成 `final`，然后重算，绑定到这个组的域名发 SET final。从 sing-box 应用新配置到重算完成之间，大约几毫秒，绑定到已删组的连接会被阻断；
  - 节点组成员、Final 的成员和选中项：不用处理，绑定按标签找出站，selector 会自己跟着变；
  - IP 规则：不影响域名的结论；
  - 改 FakeIP 网段：sing-box 会清空所有映射和绑定，所以账本也清空；
  - sing-box 重启或重载：账本不变，绑定还在 `cache.db` 里（第 4 步验证过）。
- **TTL 上限**：在 `execute_request`（UDP 和 TCP 的 53 端口共用）返回应答之前统一处理，缓存命中的应答也一样：A/AAAA 应答里只要有一个地址在 FakeIP 网段内，所有记录的 TTL 都压到 `fakeip_ttl_max`；否则压到 `direct_ttl_max`。只往下压，不往上提。没有地址的应答（空应答、NXDOMAIN、其他类型）不动。缓存里存的仍然是上游原来的 TTL，所以改上限后不用清缓存，下一次应答就按新值。两个设置存在 `atom_proxy.json` 的 `settings` 里，范围 1 到 86400 秒，不进 sing-box 配置，改了也不会触发重载。没有 `singbox` 段时（比如测试配置）不封顶。
- **已知限制**：
  - AtomDNS 进程重启后账本为空，之后改规则只影响重启后查询过的域名；不做持久化。
  - 被 LRU 淘汰的域名退回到原来的行为：等客户端重新查询。
  - 竞态：重算的同时，还在用旧规则表处理的查询可能比账本的指令晚到 sing-box，把绑定写回旧值。时间窗口就是一次查询的耗时。
  - 带客户端条件的规则加上之前，那个客户端已经拿到的 FakeIP，在过期前仍按全局绑定走（sing-box 不区分客户端）。
  - 不经过 atom API 的变化，比如 AdGuard、geosite 的在线更新，既不清缓存，也不触发重算。

### 4.6 IP 规则

目标是 IP 的规则不经过 DNS，AtomDNS 处理不了。控制面把它们编译成 sing-box 的 `ip_cidr` 路由规则，同时让 nft 把这些网段导进 sing-box。这是唯一一类在 sing-box 里匹配的规则。**[建议]**

第 3 步定下的 **[定]**：

- 带 `ip_cidr` 的规则只能单独用，不能再带域名或 `client` 条件（否则 400）。动作可以是代理、直连、屏蔽。
- AtomDNS 的 DNS 匹配跳过它们（`rules/test` 也不会命中）。启用的 IP 规则按规则表里的顺序编译成 sing-box 路由规则，排在 mixed 入站的 `resolve` 之后：代理 → `route` 到组，直连 → `route` 到 `direct`，屏蔽 → `reject`。网段写回规范形式（主机位清零）。
- 改动规则时，如果启用的 IP 规则（网段、动作、顺序）有变化，就走一遍 sing-box 应用事务（第 6 节），应用失败时规则的改动也不保存。只改域名规则不碰 sing-box。
- nft 部分（第 4 步实现，见 9.4）：nft 规则把 FakeIP 网段和 IP 规则网段导进 sing-box，网段放在 nft 集合里，由 atomdns 在每次应用成功后和启动时更新；`GET status` 的 `nft` 给出网段（分 inet4/inet6）、表名和最近一次更新的结果。只导 IPv4：IPv6 的 IP 规则只对 7890 入站有效。单网卡旁路由时，IP 规则的网段也要在主路由上加静态路由，否则只对 7890 有效。
- 没有配置 `singbox` 段时（比如测试配置），`ip_cidr` 规则仍然返回 400。

### 4.7 实现（第 2 步）

- **插件**：`atom_rules`（`rust/native-host/src/atom_rules.rs`，配置在 `atomdns/config/sub_config/atom_rules.yaml`）。它是一个 fixture，只给查询打动作标记：屏蔽 fast_mark 40、代理 41、直连 42；代理时另外把组写进 `ExecutionState::outbound`。主流程按标记分派，所以执行顺序仍写在 YAML 里。匹配复用 `matcher-core`：域名条件用 `MixSet`（full/domain 用排序池，regex、keyword 照旧），客户端用 `IpPrefixList`。规则逐条按顺序判断，第一条命中的生效。
- **位置**：重写、类型屏蔽、`unified_matcher1` 打标、IPv4 优先、DDNS 之后，原判断之前。原来排在前面的"记忆无 V4/V6"空答和黑名单屏蔽挪到规则表后面：否则一个被代理的域名如果曾经学到"无 V4"，会直接拿到空应答；直连规则也盖不过黑名单。刷新流程 `sequence_requery` 用同样的顺序。**[定]**
- **动作**：
  - 代理 → 组 X：A/AAAA 走 `$sequence_fakeip_exit`，向 sing-box 要 FakeIP，查询带 SET X，不问国内外 DNS；其他类型（MX、TXT 等）问国外 DNS，和灰名单在 `process_ot.yaml` 里的做法一样。AAAA 也发给 sing-box（同一个 SET），sing-box 只配了 IPv4 网段，返回空。
  - 直连：A/AAAA 走 `$sequence_local_exit`（国内 DNS，真实 IP），其他类型走 `$sequence_local`；兼容模式和安全模式都问国内，因为这是用户明确指定的。
  - 屏蔽：`reject 0`，即 NOERROR 空应答。**[定]**
- **审计**：命中的规则写进 `domain_set`（`规则:<备注或 ID>`）和 `matched_rule_source`（`atom_rule:<ID>`）。
- **内置规则集**：YAML 的 `builtin` 列出规则能直接引用的名字：`adguard`（`adguard_rule`，按 AdGuard 自己的屏蔽/放行匹配）、`geosite_cn`、`geosite_no_cn`（`unified_matcher1` 打标的名单，按 mapper 给它们打的 fast_mark 判断，所以 `$atom_rules` 要排在 `$unified_matcher1` 后面）。
- **自定义规则集**：
  - 远程 URL：格式 text/YAML/JSON/SRS/`.dat`（要 `category`）/`.mrs`，下载后转换成本地列表；下载走 `main_proxy`（7890）。
  - 本地列表：通过 API 编辑，条目是 `domain:`/`full:`/`keyword:`/`regexp:`，不带前缀的条目当作 `domain:`。
  - 第一次下载失败时，规则集照样创建，记下 `last_error`，内容为空。刷新失败时保留旧内容。
  - 被规则引用的规则集不能删除，返回 409 和这些规则的 ID。
- **持久化**：规则表存在 `webinfo/atom_rules.json`（`{version, rules}`），规则集存在 `webinfo/atom_rulesets.json`，都是写临时文件再改名。每次修改先校验、编译出新的匹配表，再写盘，最后整张表替换（`Live`）：查询要么看到旧表，要么看到新表，DNS 不中断。文件读不了或解析不了时，配置编译失败，程序不启动，不会悄悄换成默认表。规则表里有不合法的规则（比如带 `client` 的代理规则，见 4.2），也按文件损坏处理，停用的规则同样要校验。**[定]**
- **规则集列表文件**（codex 复审后改）：每个规则集有一个 `revision`，列表内容存在 `webinfo/atom_rulesets/<id>-<revision>.txt`。每次换列表（本地编辑、下载成功）都写一个新 revision 的文件，然后写 `atom_rulesets.json`，这一步是唯一的切换点。写元数据失败时删掉新文件，旧列表和旧元数据都还在，重启后还是旧的；成功后删掉旧文件。`revision` 为 0 表示还没有列表（远程规则集还没下载成功过，包括第一次下载失败），内容为空，也没有文件。`revision` 不为 0 而文件不存在时，按读不了处理，程序不启动。本地规则集新建时 `revision` 就是 1，所以加载时遇到本地规则集 `revision` 为 0，或者 `revision` 为 0 但 `rule_count` 不为 0，也当作状态不一致，程序不启动，不会悄悄变成空规则集。**[定]** 不为第 2 步早期没有 `revision`、文件名是 `<id>.txt` 的格式写迁移：那个格式没发布过，也没人在用；它的元数据缺 `revision`，会被上面的检查拒绝。 进程如果恰好在写完元数据、删旧文件之前退出，会留下一个没人引用的旧文件，不影响结果，目前不自动清理。
- **并发下载**：每次下载开始时领一个编号（只在内存里），并记下当时的来源（url、format、category）。下载完要写入前核对两样：编号还是这个规则集最新开始的那一次，来源也没被改掉。任何一样不对，就丢弃这次结果（成功和失败都不记录），返回 409。所以两次刷新重叠时，只有后开始的那次能写入，先开始、后结束的那次不会用旧内容覆盖新内容，也不会把成功状态改成 `last_error`。**[定]**
- **清缓存失败**：清空某个缓存时，如果写空的缓存文件失败（比如磁盘满），改为直接丢弃：先清内存，再删掉缓存文件，免得重启后又读回旧条目。丢弃也失败（文件删不掉）时，接口返回 500，说明修改已经保存并生效，但缓存没清干净。
- **group_id**（第 2 步）：校验格式：非空、没有首尾空白和控制字符、最长 64 字节。没有 `singbox` 段时只做这一项。
- **ip_cidr**：第 3 步实现，见 4.6。
- **group_id**（第 3 步）：配置了 `singbox` 段时，代理规则的 `group_id` 必须是存在的节点组或 `final`（Final），否则 400；加载时文件里引用了不存在的组同样按文件损坏处理。

## 5. 节点与节点组

### 5.1 节点 **[定]**

- 输入格式：sing-box outbound JSON、YAML、分享链接（`vless://`、`vmess://`、`trojan://`、`ss://`、`hysteria2://`、`tuic://` 等）。
- 统一转换成 sing-box outbound 后保存。保存前用 `sing-box check` 校验整份生成的配置。**[建议]**
- 分享链接的解析在 atom 的 Rust 代码里实现。**[建议]**

第 3 步定下的 **[定]**：

- 分享链接第一版支持 8 种：`vless://`（含 reality、ws、grpc、httpupgrade、h2）、`vmess://`（V2RayN 的 base64 JSON）、`trojan://`、`ss://`（SIP002、明文 userinfo 的 2022 写法、旧的整段 base64 写法；插件支持 obfs-local、v2ray-plugin）、`hysteria2://`/`hy2://`、`tuic://`、`anytls://`、`socks://`/`socks5://`/`socks5h://`（都转成 sing-box 的 socks version 5，userinfo 可以是明文 `user:pass` 或 V2RayN 的 base64）。hysteria v1 不支持，用 sing-box JSON 导入。其他协议同样先用 JSON 导入。Clash YAML 也支持 `type: socks5`（不带 TLS）。
- "YAML"指 Clash/mihomo 的 `proxies`：整份 Clash 配置、`proxies` 列表或单个节点都行，协议范围和分享链接一样，和链接共用一套转换。
- sing-box JSON：单个 outbound、数组，或整份 sing-box 配置（只取代理类型的 outbound，跳过 direct、selector 等）。`tag` 当作节点名称。
- 批量导入是全有或全无：任何一项转换失败就整体返回 400，错误里写"第 N 项"。导入的节点再经过整份配置的 `sing-box check`，不通过也整体不导入。
- 节点类型限于代理协议（socks、http、shadowsocks、vmess、trojan、naive、wireguard、hysteria、shadowtls、vless、tuic、hysteria2、anytls、ssh、tor）；outbound 不能带 `tag`（由 atom 设为节点 ID）和 `detour`（链式代理暂不提供）。
- 删除节点不需要确认：它从所有组和 Final 的成员里移除，组因此变空时按空组处理（5.2）。
- **不回显凭据**（参考 PR15 后定，codex 复审后对照 v1.14.4 的 option 定义补全）：接口返回的节点（列表、详情、导入和修改的结果）去掉所有层级里的 `password`、`username`、`user`、`uuid`、`auth`、`auth_str`、`private_key`、`private_key_passphrase`、`pre_shared_key`、TLS 的 `client_key`、REALITY 的 `short_id`（服务端只认它列出的 ID，算凭据；`public_key` 是公开的，保留），以及通用的 `key`、`psk`、`token`、`secret`（字段名按 sing-box 的 JSON 解码方式不区分大小写比较，连 `ſ`、开尔文符号这类 Unicode 折叠也算，所以 `Password`、`TORRC`、`Headers.Authorization` 一样会被去掉；拒绝 `tag`、`detour` 时也这样比较）；hysteria v1 字符串形式的 `obfs`（混淆密码）；tor 的 `torrc` 和 `extra_args` 整个不返回（里面可能以任意形式写着凭据，比如 `HTTPSProxyAuthenticator`）；`headers`、`extra_headers` 里的 `Authorization`、`Proxy-Authorization`、`Cookie`。导入时凭据字段原样保存，不去首尾空白。修改节点时可以只改名称；要改连接参数，就重新给一条链接、YAML 或 JSON（`format` + `content`，只能是一个节点）整体替换，名称不变，除非同时给了 `name`。不给内容就保留原来的连接。凭据只存在 `webinfo/atom_proxy.json` 和 sing-box 的 `config.json` 里。
- **单节点测速**：`POST nodes/{id}/delay` 走 Clash API `GET /proxies/{id}/delay`，和组测速一样，`url` 可选、超时默认 5000 毫秒、限制在 100 到 30000 毫秒之间。

### 5.2 节点组

已定：

- 类型先做 `selector` 和 `urltest`，接 mihomo 时再加 fallback 等。**[定]**
- **"默认代理"是内置组，不能删除**（ID `g-default`）。**[定]** 刚装好时它没有节点（第 3 步定）：组允许为空，生成配置时空组里放一个隐藏的 `block` 出口，这个组的流量全部拒绝，不会悄悄直连；`status` 的 `warnings.empty_groups` 和组列表的 `empty` 给出提醒。**[定]**
- **Final** 是兜底，由原判断的"国外"结果和所有没绑定的 FakeIP 流量使用，**不能设成 direct**，由 API 校验强制。**[定]** 第 3 步统一的说法：Final 是一个 selector，成员默认是所有节点组，新建的组自动加入；用户可以删减组，也可以加节点；成员里不能有 direct；同时只能选中一个成员，默认选中"默认代理"。**[定]**
- 节点和组在 sing-box 里的标签用不可变的内部 ID（如 `n-1a2b`、`g-7f3a`），名称只用于显示。这样改名不会让已有绑定失效。代价是第三方 Clash 面板里看到的是 ID。**[定]**

- **Final 做成 selector**，成员是节点组，默认选中"默认代理"。运行时通过 Clash API 切换，不用重载。`route.final` 指向这个 selector。**[定]**
- **组可以嵌套引用其他组。** 保存时做环检测，有环就拒绝。**[定]**
- **urltest 参数**：只开放测速 URL、间隔、容差三项，都有默认值，用户可以改。**[定]** 默认值沿用 sing-box 的：`https://www.gstatic.com/generate_204`、3m、50ms。**[建议]**
- **删除被规则引用的组**：API 先返回 409 和引用它的规则列表；UI 弹出确认，用户确认后带 `confirm=true` 再删一次，那些规则的动作自动改成 Final。嵌套引用了它的组，同样从成员里移除。**[定]**
- **切换后端（sing-box ↔ mihomo）**：selector 保持不变；sing-box 不支持的类型（fallback、load-balance 等）一律提示用户，并自动转换成 urltest。**[定]**

### 5.3 实现（第 3 步）

- **模块**（`atomdns/rust/native-host/src/`）：`atom_proxy.rs` 模型、校验、环检测；`atom_links.rs` 分享链接和 Clash YAML；`atom_singbox.rs` 生成配置、check、写入、重载、确认、回滚和 Clash API；`atom_rules/singbox.rs` 规则表持有者上的事务；`atom_proxy_api.rs` 接口。**[定]**
- **持久化**：`webinfo/atom_proxy.json`，内容是 `{version, nodes, groups, final, settings}`，一个版本号，写入时比较，版本不对返回 409（带当前 `version`），做法和规则表一样。文件不存在时是新安装的状态（空的"默认代理"、Final 选中它），第一次修改才写文件；文件读不了、解析不了或校验不过时程序不启动。**[定]**
- **ID**：节点 `n-` + 8 位十六进制，组 `g-` + 8 位十六进制，内置组 `g-default`，Final 的标签是 `final`。规则可以代理到 `final`（删除组时规则改用它）。
- **组**：`selector` 有 `selected`（空表示第一个成员）；`urltest` 有 `url`、`interval`、`tolerance`，新建时不填就用默认值 `https://www.gstatic.com/generate_204`、`3m`、`50`。`interval` 只接受秒、分、时（`30s`、`3m`、`1h30m`）。成员必须存在、不重复、不能是自己；组之间有环时拒绝并写出环。
- **选择和测速**：`POST groups/{id}/select`（`id` 可以是 `final`）走 Clash API `PUT /proxies/{tag}`，不重载；先切 Clash 里的选择，再保存，保存失败就把选择和配置文件都改回原来的（codex 复审后加）；atom 记下选中项，生成配置时作为 `default`。运行中的配置文件和当前状态一致时，同时把文件里的 `default` 改掉（同一个密钥，不重载），所以 `status` 仍然是一致的。urltest 组不能手动选。`POST groups/{id}/delay` 走 Clash API `GET /group/{tag}/delay`。
- **删除组**：被规则引用时返回 409 和这些规则的 ID；带 `confirm=true` 再删，这些规则改成代理到 `final`，规则表版本加 1，并清空缓存。其他组和 Final 的成员里同时去掉它；Final 选中的正是它时，回到"默认代理"（不在成员里就选第一个）。
- **改名不重载**：名称不在 sing-box 配置里。每次修改都会生成一遍配置，和运行中的文件（用它自己的密钥）比较，一样就不应用。

## 6. sing-box 配置生成 **[定]**

控制面完整生成 sing-box 的配置文件，用户不手改（和 PR15 的"部分接管"不同）。

| 段 | 内容 |
|---|---|
| `inbounds` | `dns-in`：direct，127.0.0.1:6666 UDP/TCP，用于 FakeIP DNS；`redirect-in`：TCP；`tproxy-in`：UDP；`mixed-in`：0.0.0.0:7890（端口可改）**[定]** |
| `dns.servers` | `fakeip`：inet4 网段，默认 `28.0.0.0/8`，可配置 **[定]**；`resolver`：解析节点域名用的普通上游 |
| `dns.rules` | A/AAAA → fakeip |
| `outbounds` | 节点 `n-*`、节点组 `g-*`、`direct` |
| `route.rules` | `dns-in` → hijack-dns；`mixed-in` → resolve（AtomDNS）→ FakeIP 地址走绑定或 Final，其他走 direct；IP 规则（4.6）→ 对应的组 |
| `route.final` | Final selector |
| `experimental.cache_file` | 启用，`store_fakeip: true`（锁定） |
| `experimental.clash_api` | 只监听 127.0.0.1，带随机生成的密钥 |

- **节点域名的解析**不能经过 AtomDNS，否则国外节点域名会拿到 FakeIP，形成回环。`resolver` 默认用 AtomDNS 设置里的国内上游地址，由 sing-box 直接去问；用户也可以自己指定。**[定]**
- **FakeIP 网段 `28.0.0.0/8`**：这是已分配的公网地址段（美国国防部），公网上实际不路由，社区常用来做 FakeIP。代价是真要访问这个段的真实地址时会被当成 FakeIP。**[定]**
- **mixed 入站（7890）也按规则分流。** **[定]** 浏览器通过 7890 发来的是域名，sing-box 先用 `resolve` 动作（`server` 指向本机 AtomDNS 53 端口）问 AtomDNS，再按回答走：
  - 返回 FakeIP：按域名查绑定，有绑定走对应的组，没有走 Final，目标保留域名，由代理服务器远端解析；
  - 返回真实 IP：走 direct；
  - 返回空或 NXDOMAIN：解析失败，连接被拒，等于屏蔽。
  
  AtomDNS 在返回应答之前已经把查询转给 sing-box 的 FakeIP 入口、写好了绑定，所以不存在时序问题。AtomDNS 不需要改动。直连、代理、屏蔽三种分流用原版的路由规则就能表达；"按组"这一步要在 sing-box 补丁里加一处：`matchRule` 里 resolve 完成后，如果解析出的地址属于 FakeIP，就按域名查绑定。估计 15 到 20 行。
  
  限制：每个新连接多一次本机 DNS 查询，只针对 7890 流量；AtomDNS 看到的查询来源都是 127.0.0.1，所以带"来源客户端"条件的规则对 7890 流量无效；7890 直接发来 IP 的连接，只匹配 IP 规则，没命中就走 Final。
- **应用流程**：生成 → `sing-box check` → 原子写入 → 发 SIGHUP 让 sing-box 重新加载 → 通过 Clash API 确认新实例起来了；失败就写回旧配置再重载一次。sing-box v1.14.4 的 `cmd_run.go` 收到 SIGHUP 会重新加载配置。
- 切换节点和测速走 Clash API，不需要重载。

第 3 步定下和实现的 **[定]**：

- **进程管理**：sing-box 跑在独立的 systemd 单元 `atom-sing-box.service` 里（单元文件由第 7 步的安装器写，`ExecReload=kill -HUP $MAINPID`，`Restart=on-failure`）。atom 写完配置后执行 `systemctl reload-or-restart atom-sing-box.service`：单元在运行就发 SIGHUP，没在运行（第一次）就启动它。sing-box 崩了由 systemd 拉起，atom 重启不影响代理。
- **位置**：配置和 `cache.db` 放在 atom 配置根下的 `sing-box/`。程序路径、工作目录、重载命令、Clash API 地址（`127.0.0.1:9090`）、FakeIP DNS 入站（`127.0.0.1:6666`）、本机 AtomDNS 地址（`127.0.0.1:53`）、redirect 7892 / tproxy 7893、默认 resolver 来自哪个上游组（`domestic`）、nft 表名（`atom`，第 4 步加，见 9.4）写在自带 YAML 里 `atom_rules` 插件的 `singbox` 段，不对用户开放。
- **重载失败不退出**（参考 PR15 后定，sing-box 补丁）：上游 v1.14.4 收到 SIGHUP 后先关掉旧实例；新配置如果通过了 `check` 却启动失败（端口被占用这类运行期错误），进程会直接退出，要等 atom 回滚、systemd 重启，中间代理和国外 DNS 都断。补丁让 `cmd_run.go` 记住上一份成功运行过的配置，新实例启动失败时用它在内存里把旧实例重建出来，进程不退出。atom 这边不用改：旧实例不认新密钥，确认照样失败，atom 写回旧文件再重载一次，最后运行的还是旧配置。
- **确认新配置生效**：每次应用都生成新的随机 Clash API 密钥写进配置。旧实例不认新密钥，所以用新密钥在 20 秒内拿到 `/proxies`（里面有 `final`）就证明新配置已经在跑。代价：本机接了第三方 Clash 面板的话，每次应用后要重新填密钥（Clash API 只监听 127.0.0.1）。
- **选择要同步**（codex 复审后加）：sing-box 的 selector 启动时先用 `cache.db` 里记住的选择，再看 `default`。所以每次重载并确认后，atom 用 Clash API 把每个有成员的 selector 组和 Final 切到 atom 记录的选择；同步失败按应用失败处理（回滚）。回滚时旧实例也按旧状态重新同步一遍。
- **事务**：校验（规则表和节点状态）→ 生成 → 和运行中的文件比较，一样就跳过 → `sing-box check`（配置先写到 `config.json.next`；不通过返回 400，什么都不动）→ 改名为 `config.json` → 重载 → 确认 → 写 atom 的文件 → 生效。重载或确认失败：写回旧文件（之前没有就删掉），再重载一次并用旧密钥确认，接口返回 502，错误里写回滚结果；atom 的状态和文件都不变。sing-box 已经应用但写 atom 文件失败：同样回滚 sing-box，返回 500。一次应用进行时，其他写操作返回 409（不排队）；整个进程同一时间只有一次应用。这两层保护（进程级的应用锁和规则表持有者的 busy 标记）一直持有到 atom 的文件写完；写文件失败时，回滚 sing-box 也在保护之内完成，不会把别人刚应用成功的配置改回去（codex 复审后改）。找不到 sing-box 程序时不写任何文件，直接报错。
- **两个文件的写入顺序**（codex 复审后改）：只有确认删除组会同时改规则和节点状态。先写规则（引用改成 Final），再写去掉该组的 `atom_proxy.json`；两次写之间崩溃，留下的只是一个没被引用的组，下次照常启动。第二次写失败时把规则文件写回原样，写回也失败就在错误里说明。
- **凭据文件权限**：`webinfo/atom_proxy.json` 和 sing-box 的 `config.json`（含 `config.json.next` 和写入时的临时文件）都以 0600 写入。
- **启动时**（包括 AtomDNS 每次重新加载配置后）：生成一遍配置，和运行中的文件比较；不一致（第一次启动、升级后生成规则变了、默认 resolver 跟着国内上游变了）就应用一次；失败只记日志和 `status`，不影响 DNS。
- **生成的内容**（对应上表）：`inbounds` 的 redirect/tproxy/mixed 监听 `0.0.0.0`；`dns.servers` 是 `fakeip`、`resolver`、`atomdns`（mixed 入站 `resolve` 用），`dns.rules` 把 A/AAAA 交给 `fakeip`，`dns.final` 是 `resolver`；`route.default_domain_resolver` 是 `resolver`，节点域名不会走 FakeIP；路由规则依次是 `dns-in` → `hijack-dns`、`mixed-in` → `resolve`（`server: atomdns`、`disable_cache: true`）、IP 规则（4.6）、`mixed-in` 且目标是域名（`domain_regex: ["."]`）→ `direct`；`route.final` 是 `final`；出口依次是节点、组、Final、`direct`、`block`。
- **resolver**：设置里留空时，用 `singbox.resolver_group` 那个上游组里第一个启用的 `udp://`/`tcp://` 地址条目，没有就用第一个带 `dial_addr` 的条目（`udp://dial_addr`），再没有用 `udp://223.5.5.5`。用户指定时只接受 IP、`udp://IP[:端口]`、`tcp://IP[:端口]`（域名本身还要解析）。
- **设置**（`settings`）：`fakeip_range`（IPv4 网段，前缀 8 到 24，主机位为 0，不能是回环；注意改网段会让 sing-box 清空已有的 FakeIP 映射和绑定，客户端缓存里的旧 FakeIP 失效，要等客户端重新查 DNS；这次重载如果失败，回滚到旧配置也找不回原来的映射，见 [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md) 的已知限制）、`resolver`、`mixed_port`（不能和 atom 的其他端口冲突）。改 FakeIP 网段后清空 AtomDNS 的缓存（旧网段的 FakeIP 应答）。改 mixed 端口后，如果 AtomDNS 的 `main_proxy` 正指向 `127.0.0.1:<旧端口>`，就把它改成新端口（写 `webinfo/settings.json` 并重新加载 AtomDNS 配置）；指向别的地址时不动。
- **7890 的 sing-box 补丁**：`resolve` 解析出的地址全部属于 FakeIP 时，按地址对应的域名查绑定，有就走绑定的组，没有就走 `route.final`；地址被丢掉，目标保留域名。见 [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md) 的"代理入站"。

## 7. 锁定与开放

### 7.1 锁定（fork 里写死或删掉）

| 项目 | 处理 |
|---|---|
| `dns_mode` | 固定为 FakeIP，RealIP 的代码路径删除 |
| `cn_fakeip` | 固定为关 |
| FakeIP 上游（`nocnfake`） | 固定为本机 sing-box 的 `dns-in`（`udp://127.0.0.1:6666`）。`POST /api/v1/upstream/config` 拒绝修改它，`settings.json` 里存的列表也被忽略 |
| 国外 DNS 走的代理（`$main_proxy`） | 默认是 `socks5://127.0.0.1:7890`（`settings.yaml` 里写作 `addr: "127.0.0.1:7890"`），即本机 sing-box 的 mixed 入站，经 Final 出去。可以按上游单独关闭。UDP 上游不能走 SOCKS（AtomDNS 现有规则），所以默认的国外上游用 DoH/DoT/TCP。不用 nft 在 OUTPUT 链导流：那样要处理本机流量、同步上游 IP 集合，还要防回环。**[定]** |
| 专属分流组 | 删除。第 1 步在产品配置里删掉（不 include `special_groups.yaml`，主流程去掉 `$special_upstream_matcher`、`$sequence_special`）；程序只在配置 include 了这个文件时才启用专属分流组，所以这部分 Rust 代码现在不会执行，和原有的规则名单 API 一起在第 6 步删除 |
| `realiplist`、`cnfakeipfilter` | 删除（"代理但返回真实 IP"在 FakeIP 网段分流架构下等于悄悄直连）**[定]**。核实过的语义：`realiplist`（mark 9）在所有名单里优先级最高，A/AAAA 和其他类型都问国外 DNS（经 `main_proxy`），返回国外真实 IP，流量不进 sing-box；`cnfakeipfilter`（mark 10）只在 `cn_fakeip` 开启时生效。两个名单、mapper 条目、名单文件、分流分支和只给 realiplist 用的 `sequence_google_exit` 都已删除。以后需要"国外 DNS 解析后直连"，在规则表里表达 |
| AtomDNS 原有的在线更新（程序、配置包、WebUI） | 删除，改为产品自己的更新渠道。第 1 步删掉了整个 `update` 模块（含 `update-guard` 子命令、中断恢复、`/api/v1/update/*`、`/api/v1/config/export`），容器模式（`ATOMDNS_CONTAINER`，用镜像里的配置包初始化卷）依赖配置包，一起删了；`docker/` 和发布脚本还没清理 |
| AtomDNS 原有的 WebUI 和规则名单 API | 不再提供 |
| YAML 配置 | 由产品自带，用户不改。放在 fork 的 `config/`，来自 `jasonxtt/file` 的 `atomdns/config/full/config_rust`（v0.6.4，schema 7），改动见 `config/README.md` |
| sing-box `cache_file.store_fakeip` | 固定开 |
| 管理 API 监听地址 | 监听局域网，暂不做登录鉴权 **[定]** |

### 7.2 开放（通过控制面 API 修改）

`core_mode`（兼容/安全模式）、类型屏蔽、IPv4 优先、缓存开关和容量、国内外上游 DNS 列表、ECS、域名学习、FakeIP 网段（改了要连同 nft 规则一起重新生成）、直连应答和 FakeIP 应答的 TTL 上限（第 5 步实现：`PUT settings` 的 `direct_ttl_max`、`fakeip_ttl_max`，见 4.5）。

### 7.3 跨组件的一致性

由控制面一处修改、同时下发到各组件：

- FakeIP 网段：sing-box 的 `inet4_range`、nft 集合（atomdns 更新，9.4）、主路由静态路由（文档提示用户配置）。
- IPv6：v0 不做 IPv6 FakeIP；被代理域名的 AAAA 查询返回空应答。**[建议]**
- IP 规则网段：sing-box 路由规则和 nft 集合（atomdns 更新，9.4）；单网卡旁路由时还有主路由静态路由（提示用户配置）。

## 8. 管理 API（骨架）**[建议]**

正式规范用 OpenAPI 3 写在 `docs/api/openapi.yaml`，WebUI 和第三方 UI 都按它来实现。路径前缀暂定 `/api/v1/atom/`，和 AtomDNS 现有的 API 分开。

| 资源 | 操作 |
|---|---|
| `rules` | 列表、新建、修改、删除、`POST rules/move`（拖动）、`POST rules/test`（输入域名，看会命中哪条规则） |
| `rulesets` | 列表、新建（内置 geosite / 远程 URL / 本地列表）、刷新远程、删除 |
| `nodes` | 列表、导入（`format: json|yaml|link`，可以批量）、修改、删除 |
| `groups` | 列表、新建、修改、删除（"默认代理"不可删）、`POST groups/{id}/select`、`POST groups/{id}/delay` |
| `final` | 读取、设置（成员和选中项，选中项只能是一个） |
| `settings` | 读取、修改 7.2 里开放的项 |
| `status` | 两个进程的版本、运行状态、最近一次应用 sing-box 配置的结果 |
| `apply` | 查询最近一次变更的应用结果（见下） |

- 写操作即时生效。每次写入是一个事务：校验 → 生成 → `sing-box check` → 写入 → 重载 → 提交；任何一步失败就整体回滚，返回明确的错误。
- `rules` 和 `rulesets`（第 2 步实现，`rust/native-host/src/atom_rules_api.rs`）：
  - `GET rules` 返回 `{version, rules}`。规则的 JSON：`{id, enabled, remark, match: {domain, suffix, keyword, regex, ruleset, client, ip_cidr}, action: {type: proxy, group_id} | {type: direct} | {type: block}}`。
  - `POST rules` 新建，可以带 `before_id`/`after_id` 指定位置，默认放最后。`PUT rules/{id}` 整条替换。`DELETE rules/{id}?version=N` 删除。
  - `POST rules/move {id, before_id | after_id, version}` 拖动，`version` 必填。其他写操作的 `version` 可选，带了就检查。版本不对时返回 409，错误体里带当前 `version`。每次规则变化，版本加 1。
  - `POST rules/test {domain, client?}` 返回命中的规则（`matched`、`index`、`action`、`group_id`、`rule`），没命中时返回 `{matched: false, fallthrough: "original"}`，即交给原判断。只看规则表，不模拟系统固定项和原判断。
  - `GET rulesets` 列出内置集合（`type: builtin`）和自定义规则集；`GET rulesets/{id}` 带上列表内容。`POST rulesets` 的请求体是 `{name, type: remote, url, format?, category?}` 或 `{name, type: local, rules}`。`PUT rulesets/{id}` 改名、改地址或替换本地列表。`DELETE rulesets/{id}` 删除。`POST rulesets/{id}/refresh` 重新下载。
  - 错误体都是 AtomDNS 现有的 `{"error": "..."}`。写操作和 `rulesets` 的下载都占用管理租约，和设置、配置重载互斥（租约只防配置重载，不让写操作排队）。
  - 带 `client` 的代理规则返回 400（4.2）。下载期间规则集的来源被改掉，或者之后又开始了一次下载，返回 409，这次结果丢弃。修改已生效但缓存没清干净时返回 500（4.7）。
- 节点、组、Final、设置、`status`、`apply`（第 3 步实现，`rust/native-host/src/atom_proxy_api.rs`）：
  - `GET nodes` 返回 `{version, nodes}`，每个节点带 `id`、`name`、`type`、`server`、`server_port`、去掉凭据的 `outbound`（5.1）和所在的组 `groups`。`POST nodes {format, content}` 导入，`content` 是字符串（`json` 格式也可以直接给 JSON 值），返回 201 和新节点。`PUT nodes/{id} {name?, format?, content?}`（`format` 和 `content` 要一起给，整体替换连接），`DELETE nodes/{id}?version=N`，`POST nodes/{id}/delay {url?, timeout?}` 返回 `{id, delay}`。
  - `GET groups` 的每个组带 `empty`、`builtin`，selector 带当前选中 `now`。`POST groups {name, type: selector|urltest, members, selected?, url?, interval?, tolerance?}` 新建，`PUT groups/{id}` 整组替换，`DELETE groups/{id}?confirm=true`（见 5.3），`POST groups/{id}/select {name}`，`POST groups/{id}/delay {url?, timeout?}`（毫秒，默认 5000）返回 `{delays: {成员: 毫秒}}`。
  - `GET final` 返回 `{version, members, selected, empty}`。`PUT final {members?, selected?}`：成员变了走应用事务；只改选中项走 Clash API。
  - `GET settings` 返回 `{version, fakeip_range, resolver, resolver_effective, mixed_port, direct_ttl_max, fakeip_ttl_max}`，`PUT settings` 改其中任意几项（TTL 上限是第 5 步加的，见 4.5）。
  - `GET status`：`atomdns.version`；`singbox.running`、`version`（Clash API 的 `/version`）、`in_sync`（运行中的文件是否就是当前状态生成的）、`host`；`last_apply`；`bindings`（绑定账本，第 5 步，见 4.5.1）；`warnings.empty_groups`；`nft`（4.6、9.4：`fakeip_range`、`ip_cidr`、表名 `table`、最近一次更新集合的结果 `sync`）。`GET apply` 返回 `last_apply` 和 `in_sync`，`POST apply` 用新密钥重新应用一次。`last_apply` 是 `{at, trigger, ok, changed, error?}`，`trigger` 是 `nodes`、`groups`、`final`、`settings`、`rules`、`apply`、`startup` 之一，在进程内跨配置重载保留。
  - 写操作都可以带 `version`（请求体，DELETE 用 `?version=`），和节点状态的版本比较。sing-box check 不通过 400，重载或确认失败 502（已回滚），一次应用进行中 409。
  - 写操作占用管理租约，`PUT settings` 例外：它自己拿租约，只覆盖 sing-box 的部分，因为改 mixed 端口要重新加载 AtomDNS 配置，而重新加载要等所有租约结束。
- 错误格式沿用 AtomDNS 现有的格式。**[定]**
- 暂不做鉴权。**[定]**

## 9. 部署

### 9.1 形态 **[定]**

DNS 和代理在同一台 VM 上。客户端的 DNS 指向这台 VM；FakeIP 网段的流量想办法送到这台 VM。

| 拓扑 | 流量怎么进来 | 说明 |
|---|---|---|
| 单网卡旁路由（和主路由在同一网段） | 主路由加一条静态路由：FakeIP 网段 → VM | 普通用户最常见的情况 |
| 双网卡网关（LAN/WAN，参考 xray 那台机器） | VM 本身就是网关 | LAN 的全部流量都经过这台 VM：FakeIP 和 IP 规则网段进 sing-box，其余流量 NAT 后从 WAN 出去。安装器必须把这一点明确告诉用户。**[定]** |

VM 上：TCP 用 redirect，UDP 用 TProxy，只处理目标是 FakeIP 网段和 IP 规则网段的流量。sing-box 自己发出去的流量目标是真实 IP，不会被再导回来。安装器检测网卡数量，生成对应的 nft 规则。具体规则写在单独的部署文档里，参考 xray 那台机器现有的规则。

### 9.2 安装器

- 下载配套版本的 atom 和 sing-box，安装 systemd 服务、默认配置、WebUI。
- 配置 nft、策略路由（TProxy 需要 fwmark 和本地路由表）、`ip_forward`。
- 提示用户：在主路由上添加 FakeIP 静态路由；把 DHCP 下发的 DNS 改成这台 VM。

### 9.3 不处理的绕过路径

浏览器开启 DoH、IPv6 RA 下发了别的 DNS，这类客户端拿不到 FakeIP。客户端的网关是主路由，旁路 VM 屏蔽这些也没用，所以不处理。**[定]**

v0 不做 IPv6 FakeIP，被代理的域名只返回 IPv4。

### 9.4 nft 和策略路由（第 4 步）**[定]**

具体规则、端口、标记和实测环境见 [deploy-nft.md](deploy-nft.md)。

- **规则**：nft 表 `inet atom`，两个集合 `fakeip4`（FakeIP 网段）、`iprule4`（启用的 IP 规则的 IPv4 网段），`flags interval; auto-merge`。`prerouting` 上只看局域网网卡进来、目标不是本机地址的包：目标在集合里的 TCP redirect 到 7892，UDP TProxy 到 7893 并打标记 `0x233`；策略路由 `fwmark 0x233 lookup 233`，表 233 是 `local 0.0.0.0/0 dev lo`。双网卡网关另加一张 `ip atom_nat` 表（LAN → WAN 的 masquerade）和 `ip_forward = 1`；单网卡旁路由不需要 `ip_forward`。
- **sing-box 自己的流量**：nft 只挂 `prerouting`，本机进程的包走 `output`，不会被导回来，所以不设 `routing_mark`、也没有排除标记。本机进程访问 FakeIP 不处理。
- **集合由 atomdns 维护**（用户选的，没有放进应用事务）：`atom_rules.yaml` 的 `singbox.nft_table: atom`（没有就不碰 nft），`singbox.nft` 是命令（默认 `[nft]`）。sing-box 应用成功后（FakeIP 网段和 IP 规则只随应用变化）、`POST apply` 后、每次启动和重新加载配置时，用 `nft -f` 执行一个脚本，两个集合 flush 后重新写入，一个事务。更新在应用锁里做，不会乱序。失败不回滚规则的改动，只记日志和 `status.nft.sync`（`{at, ok, error?, sets}`）。集合的内容取自正在运行的 sing-box 配置文件 `config.json`（FakeIP 服务器的 `inet4_range` 和路由规则里的 IPv4 `ip_cidr`），文件读不到时才用 atom 的状态；每次应用尝试（成功、应用失败回滚、保存失败补偿、之后编译失败）都在应用锁里以同步一次结束（codex 第 1、2 轮复审后改）。已知限制（codex 第 3 轮，主会话决定不改代码）：补偿时旧文件写回成功、但重载命令失败，sing-box 仍在跑新配置，而 nft 按写回的旧文件更新；要同时出现保存失败和重载失败才会发生，接口会报"已写回旧配置，但重载后没有确认"，`in_sync` 显示不一致，再应用一次（或 `POST apply`）就恢复。见 deploy-nft.md 第 8 节。规则文件里的集合是空的，每次加载都整表替换，所以 atomdns 的单元先 `ExecStartPre` 加载规则文件和策略路由，启动后自己填集合（安装器的写法，第 7 步）。实现在 `atom_singbox.rs`（`NftSets`、`nft_script`、`sync_nft`）和 `atom_rules/singbox.rs`（`sync_nft`、`last_nft`）。
- **UDP 53 的写回**（联调发现，用户选的修法）：sing-box 的 TProxy 回包要用 IP_TRANSPARENT 绑定"原目标地址:端口"，atomdns 在 `*:53` 监听且没设 `SO_REUSEADDR` 时，客户端发往 FakeIP 或 IP 规则地址的 UDP 53（比如 IP 规则里有 8.8.8.8，客户端直接问它）绑定报 `EADDRINUSE`，回包发不出去。改为 atomdns 在通配地址（`[::]`、`0.0.0.0`）上的 UDP 监听设 `SO_REUSEADDR`（`rust/native-host/src/bind.rs`）。代价：同一台机器上再起一个监听通配地址的 atomdns，不会再报端口被占用，而是共用 53 端口；具体地址的监听照旧拒绝重复绑定。
- **拓扑**：单网卡旁路由和双网卡网关都在 netns 里实测过（deploy-nft.md 第 7 节）。单网卡时主路由只看到去程，转发链里"丢弃 invalid"的规则可能丢包，这一点没有实测条件，文档给了两种处理办法。

## 10. 仓库与版本

```text
atom/
  README.md
  docs/        设计、补丁说明、API 规范、部署文档
  atomdns/     AtomDNS fork（独立 git 仓库，分支 atom/main；自带的 YAML 配置在 atomdns/config/）
  sing-box/    sing-box fork（独立 git 仓库，分支 atom/fakeip-outbound）
  installer/   安装器（以后）
```

- fork 自带的 YAML 配置放在 `atomdns/config/`：从 `jasonxtt/file` 的 `atomdns/config/full/config_rust`（v0.6.4，schema 7）复制过来，锁定改动直接改这份，不动 file 仓库，改动记在 `atomdns/config/README.md`。**[定]**
- `atom/` 建一个新仓库，先只在本地用 git，差不多了再推到 GitHub。`atomdns/` 和 `sing-box/` 是独立的 git 仓库，在 `atom` 的 `.gitignore` 里排除。**[定]**
- 三个组件按"配套版本"一起发布，安装器和在线更新只认配套的组合。**[建议]**

## 11. 实施顺序 **[建议]**

1. **AtomDNS fork 打底**（已完成）：从 main 建分支；锁定 7.1 里的项，删除 RealIP、专属分流组和在线更新；加上 EDNS0 65001 编码，Final 路径发 CLEAR。验收：沿用现有名单，链路能跑通，FakeIP 带上 CLEAR。
2. **规则表**（已完成，实现见 4.7）：模型、首条命中匹配插件、`rules` API、拖动、`rules/test`；规则命中"代理"时发 SET。另外做了 `ruleset` 条件的全部三种来源（内置、远程 URL、本地列表）和 `rulesets` API。**[定]** 还没做：远程规则集的定时自动更新（现在只能手动 `refresh`）。
3. **sing-box 配置生成**（已完成，2026-11-09 收尾，经过三轮 codex 复审；实现见 4.6、5.3、第 6 节）：节点导入（JSON/YAML/链接）、节点组、Final、事务式应用和回滚；7890 按规则分流，包括 sing-box 补丁里 resolve 之后查绑定的那一处。还没做：nft 规则（第 4、7 步）、systemd 单元文件（第 7 步）。
4. **联调**（已完成，2026-11-09；实现和定下的事见 4.6、9.4，规则见 [deploy-nft.md](deploy-nft.md)）：在 mosdns-rust 上用临时 systemd 单元跑 atomdns 和补丁版 sing-box，客户端用 netns 模拟，单网卡旁路由和双网卡网关两种形态都实测。验证通过的：DNS（代理规则 → FakeIP 且 sing-box 收到 SET，AAAA 空应答也带 SET；直连 → 真实 IP；屏蔽 → 空应答；没命中的国外域名 → FakeIP + CLEAR；国内域名 → 真实 IP；EDNS 选项用抓包核对）；TCP 经 redirect、UDP 经 TProxy（NTP、DNS）进 sing-box，从绑定的组或 Final 出去，UDP 回包源地址是 FakeIP；IP 规则（TCP、UDP、屏蔽，启用/停用时 nft 集合跟着变）；7890 按规则分流；拖动排序和改组立即生效，删掉代理规则后下一次查询重新发 CLEAR（缓存已清）；sing-box 重启后不重新查 DNS 绑定仍在；atomdns 重启后规则、节点、组、Final 不变，启动检查不重载 sing-box，nft 表重新加载并填好；`sing-box check` 不通过返回 400 什么都不动；mixed 端口被占时重载失败返回 502 并回滚，sing-box 进程不退出（PID 不变），期间 7890 请求 40/40 成功；网关形态下其他流量经 NAT 转发、不进 sing-box。联调中修的：UDP 53 写回（9.4）。codex 复审三轮：第 1、2 轮的两条 MAJOR（应用失败回滚、保存失败补偿后 nft 没跟上运行中的配置）已修；第 3 轮的一条（补偿时重载也失败）按已知限制处理，见 9.4 和 deploy-nft.md 第 8 节。没做：真实主路由下的单网卡形态、真实节点。
5. **绑定账本和缓存失效**（已完成，2026-11-09；实现见 4.5.1）：绑定账本（SET 和 CLEAR 都记，LRU 16384 条，规则或规则集改动、确认删组后同步重算，有变化的直接发给 sing-box 的 `dns-in`，改 FakeIP 网段时清空）；直连应答 TTL 上限 `direct_ttl_max`（默认 300 秒）和 FakeIP 应答 TTL 上限 `fakeip_ttl_max`（默认 1 秒）。验证（mosdns-rust，第 4 步的临时环境，atomdns 换成第 5 步的调试版，通过运行时的 systemd drop-in 指向 `/root/atom-step5/bin`，没有永久替换）：客户端拿着 FakeIP 不重新查询，规则从组 X 改成组 Y 后连接从 Y 出去，抓包看到账本发出 SET Y；删掉代理规则后 sing-box 收到 CLEAR，同一个 FakeIP 走 Final；代理改直连、改屏蔽分别发 SET direct、SET block，同一个 FakeIP 立即直连、被拒；原判断走 Final 的国外域名加了代理规则后，同一个 FakeIP 改走新组；确认删组后发 SET final；sing-box 重启后绑定仍在；改 FakeIP 网段后账本清空；FakeIP 应答 TTL 是 1，改 `fakeip_ttl_max` 后跟着变；直连应答（上游 TTL 86400）封顶到 300，改 `direct_ttl_max` 后跟着变，UDP、TCP 和缓存命中都一样；改 TTL 设置不重载 sing-box。全量 `cargo test --workspace` 1635 个通过、0 失败。
6. **简易 WebUI + OpenAPI 规范定稿**。
7. **安装器**。

每一步都单独验收后再进行下一步。实施顺序已认可。**[定]**

## 12. 审阅结论（2026-11-08）

- 第 12 节原来的 6 个问题都已答复，答复已写回上面对应的章节。
- 没有单独提到的 **[建议]** 项，用户都认可，按建议执行。
- 国外 DNS 走 SOCKS（mixed 7890），见 7.1；7890 的流量也按规则分流，见第 6 节，在第 3 步实现。**[定]**
- 目前没有等待确认的项。
