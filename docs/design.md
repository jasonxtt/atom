# atom 设计文档 v0

状态：v0 已审，第 12 节的问题已答复；第 2 步实现时新定的事项写回了第 3、4、8 节，第 3 步的写回了第 4.6、5、6、8 节，第 4 步的写回了第 4.6、6、7.3、9.4、11 节，第 5 步的写回了第 4.5、7.2、8、11 节，第 6 步（6a）的写回了第 3.2、4.4、4.5.1、7.1、7.2、8、11 节，第 7 步的写回了第 6、7.1、9.2、10、11 节；2026-11-12 的规则条件和 nft 改动写回了第 3.3、4.2、4.6、6、7.3、8、9.1、9.2、9.4 节；2026-11-13 开源写回了第 2、10 节。日期：2026-11-13。

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
| atom | `atom/atomdns`（仓库 `jasonxtt/atom-core`）：从 `jasonxtt/atomdns` main 分出 **[定]**；2026-11-12 合入上游 v0.6.6（DoH over HTTP/3、DoQ 上游） | DNS 决策、规则表、控制面、管理 API、WebUI 托管 |
| sing-box | `atom/sing-box`（仓库 `jasonxtt/sing-box`）：分支 `atom/fakeip-outbound`，基于 v1.14.4 **[定]** | FakeIP、出口绑定、节点协议、节点组选择和测速 |
| 安装器 | `atom/installer` **[定]** | 一条命令加网页向导：安装两个程序和 systemd 服务，配置 nft、路由和 `ip_forward`，升级和卸载（9.2） |

- 两个进程之间只通过 DNS 和 HTTP 通信，不共享内存，也不用 cgo。
- fork 和 AtomDNS 主线差得多没关系，好用优先。**[定]** 主线的修复按需合并过来，不承诺跟进。
- Rust 编译按 AtomDNS 的规矩放到远端机器 `mosdns-rust` 上做。sing-box 的 Go 编译量小，可以在本地做。**[建议]**

## 3. DNS 决策流程

### 3.1 查询路径

```text
1. 系统固定项        重写/hosts、本地 PTR、类型屏蔽（SOA/PTR/HTTPS）       不可拖动
2. 用户规则表        有序，第一条命中就生效，可拖动                      见第 4 节
     代理 → 组 X     直接返回 FakeIP，EDNS SET X；不问国内外 DNS
     直连            问国内 DNS（或规则指定的上游组），返回真实 IP
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
| 3 `unified_matcher1` 名单打标 | 用户维护的名单改由规则表表达：`realiplist`、`cnfakeipfilter` 第 1 步删除；黑名单、白名单、灰名单第 6 步删除，用户自己写规则；`cuscn`、`cusnocn` 第 6 步成为规则表的内置规则集（4.4）。`geosite_cn` / `geosite_no_cn` 和学习结果留给第 3 层的原判断 **[定]** |
| 4/6 专属分流组 | 删除（第 1 步删配置，第 6 步删代码）**[定]** |
| 5 黑名单、广告 | 广告变成规则表里的普通规则，可以拖动（见 4.4）；第 2 步删掉了固定的广告屏蔽步骤和 `block_ads` 开关。黑名单屏蔽第 6 步删除（4.4）**[定]** |
| 7 DDNS | 第 6 步保留为系统固定项（名单在控制面编辑）；第 6b 步删除，写成规则：直连 + 上游组 + 不缓存，放在规则表最前面（4.8）。名单、mark 6 的三支分支、`ddns_foreign` 开关和 `ddns` 接口都删掉 **[定]** |
| 8 指定客户端直连 | 变成规则条件"来源客户端"（见 4.2）；`client_routing` 开关、`client_ip` 名单和这两个分支第 6 步删除 **[定]** |
| 9 全局缓存 | 保留，排在规则表后面；规则变化时清空（见 4.5.1 的"清缓存"） |
| 10/11 名单内、名单外 | 保留，成为第 3 层的原判断 |

现有的 `domain_mapper` 是把所有命中名单的 mark 合并起来，不是"第一条命中"。规则表需要新写一个匹配插件，可以复用 `matcher-core` 里的 trie、keyword 和 regex 实现。**[建议]**

### 3.3 EDNS0 65001

协议见 [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md)。

- 只在发往本机 sing-box FakeIP 入口的查询上附加；其他上游不带这个选项，避免泄露到公网。
- 先剥掉客户端自带的 65001 选项，再写入自己的指令。
- 规则命中"代理 → 组 X"时发 SET X；原判断的结果是国外时发 CLEAR，由 `route.final` 处理。所以大部分域名不会在 sing-box 里留绑定记录。**[定]**
- CLEAR 的含义（2026-11-12，用户定）**[定]**：走 Final，不经过路由规则。DNS 判定走代理、给了 FakeIP 的连接一定走代理：有绑定走规则指定的组，没有绑定走 Final，sing-box 的路由规则（IP、端口、网络规则）一概不参与。三个入口一样：redirect、TProxy（含 PreMatch）还原 FakeIP 时、mixed 入站 `resolve` 得到 FakeIP 时。原来的补丁是"有绑定才跳过路由规则"，没绑定的 FakeIP 连接会先过路由规则，比如一条端口规则会把没命中规则的国外域名截走；没有改成给每个国外域名发 SET final，那样绑定会越存越多，也挤占绑定账本。见 [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md)。
- 实现（第 1 步）：在 fork 的 `rust/native-host/src/edns_directive.rs`，挂在 `ForwardCatalog` 发往上游的出口上，所有上游查询都经过这里。发往 `nocnfake` 组的查询：去掉客户端带的 65001，加上 CLEAR（`[1,2]`）；客户端没有 OPT 时补一个 OPT（UDP 大小 1232）。发往其他组的查询：没有 65001 时原样发出；带了就去掉，其他 option（ECS、cookie 等）保留。剥 65001 只按 OPT 的结构逐个遍历选项，不依赖 ECS 等选项的语义校验（比如重复 ECS 时照样剥）；附加记录连结构都解析不了的查询，对所有上游都拒绝，不原样发出。`nocnfake` 的查询还要通过完整的 OPT 校验，过不了就报错，不发给 sing-box。改写过的查询一律用展开后的域名重建问题段，因为原包里的压缩指针可能指向后面会移动的选项字节。
- 注意：FakeIP 应答会进全局缓存（`cache_all`），命中缓存时不再问 sing-box，也就不会重发 CLEAR/SET。第 1 步只有 CLEAR，不受影响；第 2 步发 SET 以后，规则变化要按 4.5 清缓存。
- SET 的实现（第 2 步）：规则表插件命中"代理 → 组 X"时，把 X 写进查询状态（`ExecutionState::outbound`，在 `sequence-core`）；`ForwardCatalog` 发往上游时把它交给 `edns_directive.rs`，发往 `nocnfake` 的查询写 SET X，没有 X 时写 CLEAR，其他上游不带。规则表排在 `cache_all` 前面，命中代理规则的查询不经过全局缓存，每次都会把 SET 发给 sing-box；规则每次变化都清空全局缓存（4.5.1 的"清缓存"），所以删掉代理规则后，`cache_all` 里不会留着旧的 FakeIP 挡住 CLEAR。**[定]**
- 附加记录的 owner 名（第 2 步修）：先按问题段的规则校验展开后的名字（压缩指针不能越界、不能自指、不能成环），再接受这条记录；以前只是跳过 owner，这类 TYPE=OPT 的记录会被原样发给普通上游。

## 4. 规则表 **[定]**

### 4.1 模型

```text
Rule {
  id          不可变 ID
  enabled     是否启用
  remark      备注（显示用）
  match       匹配条件，见 4.2（client_invert：客户端条件取反，第 6 步）
  action      proxy(group_id) | direct | block
  dns         直连规则可选：上游组（第 6b 步，4.8）
  cache       直连规则可选：false 时不经过任何缓存（第 6b 步，4.8）
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
| `ip_ruleset` | 取规则集的 IP 部分（第 6b 步，4.6、4.9）：`ruleset` 取域名部分，两个都填就是整个规则集 |
| `port` / `port_range` | 目标端口、端口范围（`起始:结束`，一边可以空），由 sing-box 匹配，只管真实 IP 的连接（2026-11-12，见 4.6） |
| `network` | `tcp`、`udp`，由 sing-box 匹配，同上 |

一条规则内部是"或"还是"与"：**[定]** 同一类条件之间是"或"，不同类之间是"与"。比如"suffix google.com 或 youtube.com，并且客户端是 192.168.1.50"。

"类"的划分（第 2 步实现时定下）：`domain`、`suffix`、`keyword`、`regex`、`ruleset` 都是域名条件，算同一类；`client` 是第二类；IP 部分（`ip_cidr`、`ip_ruleset`）由 sing-box 匹配（4.6）。所以"domain a.com，ruleset 广告"表示 a.com 或广告域名。只有 `client` 条件的规则匹配这个客户端的所有域名。规则至少要有一个条件。

端口和网络条件（2026-11-12，用户定）**[定]**：`port`、`port_range`、`network` 和 IP 部分一起编成 sing-box 路由规则，语义同 sing-box 无头规则：目标端口（`port` 和 `port_range` 算一类）、网络、IP 部分之间是"且"，每一类里的多个值是"或"（所以"IP 网段 A 或 B，且 TCP，且端口 443 或 8000-9000"）。DNS 查询时不知道端口，所以带端口或网络条件的规则不能再带域名类条件或客户端条件（400）；动作可以是代理、直连、屏蔽。它们只管真实 IP 的连接（方案 A）：FakeIP 连接在 sing-box 里按绑定或 Final 走，不经过路由规则（3.3）。

所以一条规则里，域名类条件之间是"或"；客户端和域名类之间是"且"；IP 部分和域名类之间按同一动作分别在 sing-box 和 AtomDNS 里匹配，效果上是"或"；端口、网络和 IP 部分之间是"且"。规则编辑页按这个关系在条件块之间标小字"或""且"（块按 域名类、IP 部分、端口、网络、客户端 的顺序显示）。

界面的叫法（2026-11-12，用户定）：规则编辑页的条件分"内置规则集"（MetaCubeX、blackmatrix7 目录，原来叫"规则源"）、"自定义规则集"和"自定义条件"（域名、目标 IP 网段、目标端口、网络、客户端，原来叫"匹配器"）；规则集页原来叫"内置"的 `cuscn`、`cusnocn`、`adguard`、`geosite_cn` 等叫"默认规则集"。

粘贴 sing-box 规则：做过（规则编辑页粘贴一条无头规则 JSON，在前端拆成条件），用户看过界面后觉得碍眼，2026-11-12 去掉了。不开嗅探，不加需要嗅探的条件。

客户端条件取反（第 6 步，用户定的）**[定]**：`match.client_invert` 为 true 时，"客户端不在 `client` 列表内"才算命中。只能和 `client` 一起用，单独给返回 400；和域名条件之间仍然是"与"；动作仍然只能是直连或屏蔽。一条规则一个动作，不命中就继续往下匹配；不做"全部代理"。AtomDNS 原来的两种客户端分流模式用规则写：

- "名单内客户端不走代理"（`listed_direct`）= "client ∈ 名单 → 直连"；
- "仅名单内客户端走代理"（`only_listed_proxy`）= "client ∉ 名单 → 直连"（`client_invert: true`）。

这条规则放在规则表最前面，这些客户端就完全不分流（连屏蔽规则也不生效）；放在某条屏蔽规则下面，那条屏蔽对它们仍然有效。没有客户端地址的查询（绑定账本的重算；进程内的查询，即 requery 和重写目标的解析，原来带的是伪造的 127.0.0.1，codex 第 4 轮后改成不带客户端）任何客户端条件都不命中，取反的也一样：没有客户端时谈不上"不在名单内"，而且这类规则只能直连或屏蔽，不影响 sing-box 的绑定（4.5.1）。`rules/test` 的 `client` 参数照样能测出取反的效果。

带 `client` 条件的规则，动作只能是直连或屏蔽，不能是代理。**[定]** 原因：sing-box 只按域名绑定出站组，不区分客户端。比如"客户端 A 的 foo.com → 组 X"：A 一查就发 SET X，于是别的客户端拿到 foo.com 的 FakeIP 后也走 X。别的客户端从缓存拿到 FakeIP 时不发 CLEAR；不走缓存时，A 的 SET 和别人的 CLEAR 会来回覆盖。所以代理规则对所有客户端都一样，客户端条件只用来决定这个客户端能不能走代理（直连或屏蔽）。sing-box 不增加按客户端分流的功能。API 新建或修改这种规则时返回 400。

### 4.3 拖动排序

- 每条规则有显式的顺序。API 提供移动操作：`{id, before_id | after_id, version}`。
- 规则表整体带一个版本号，写入时比较版本，防止两个人同时编辑时互相覆盖。
- 修改后重新编译匹配器并原子替换，不用重启，DNS 不中断。

### 4.4 广告和黑名单

广告（AdGuard 在线规则）和黑名单也是规则表里的普通规则，动作是"屏蔽"，可以拖动。用户要放行某个误杀的域名，把一条"直连"规则拖到它前面就行。

新安装时的默认规则表 **[定]**：

1. 代理修正 → 代理到 Final（`ruleset: cusnocn`：geosite-cn 里其实在国外的域名）
2. 直连修正 → 直连（`ruleset: cuscn`：geolocation-!cn 里其实在国内的域名）

原来前面还有两条，2026-11-14 用户定都去掉 **[定]**：

- "局域网和私有域名 → 直连"（后缀 `lan`、`local`、`localhost`、`localdomain`、`home.arpa`、`internal`）。去掉以后这些域名和其他域名一样走原判断；要直连或交给指定的上游组（比如本地路由器），自己写一条规则。
- "广告 → 屏蔽（`ruleset: adguard`）"：`adguard` 的来源是原作者自己的几份名单，界面上看不到也改不了。内置规则集 `adguard` 仍然保留在规则集页（可以删、可以被规则引用）；要屏蔽广告，从目录里选规则集写一条屏蔽规则。已经写过 `atom_rules.json` 的安装不受影响（规则表照旧）。

其余全部交给原判断。`atom_rules.json` 不存在时使用默认规则表，第一次修改时才写文件（`atomdns check` 不会写）。配置里没有对应的内置规则集时，那一条不放。

自带名单并入规则表（第 6 步）**[定]**：

- 黑名单、白名单、灰名单删除：插件、`unified_matcher1` 条目、名单文件、原判断里的分支和 `block_blocklist` 开关都删掉。这三份是原作者个人的名单，分享版不带；要屏蔽、代理、直连某些域名，直接写规则。原来黑名单返回 NXDOMAIN，规则表的屏蔽是空应答。
- `cuscn`、`cusnocn` 是公共的修正列表（nekolsd 的 sing-geosite），配置里带着 srs 文件，所以做成内置规则集（`atom_rules.yaml` 的 `builtin`，和 `geosite_cn` 一样按 mapper 打的 fast_mark 判断），不做成远程规则集：新装时还没有节点，经 7890 下载会被空的默认组拒绝。它们在原判断里排在 geosite 前面，现在是默认规则表的两条；原判断里的 13、15 分支删除。原来 `cusnocn` 发 CLEAR 走 Final，现在发 SET `final`，出口一样。
- 不写迁移：atom 还没发布过。
- 内置规则集可以重新下载：`POST rulesets/{id}/refresh` 对 `adguard`、`geosite_cn`、`geosite_no_cn`、`cuscn`、`cusnocn` 有效（`GET rulesets` 里 `refreshable: true`），下载完照常清缓存、重算账本。
- 第 6b 步 **[定]**：`geoip_cn` 也列在规则集里（`atom_rules` 的 `ip_builtin`），可以刷新、改地址，规则不能引用（`usable: false`）。`geosite_cn`、`geosite_no_cn`、`geoip_cn` 是原判断必需的，不能删；`adguard`、`cuscn`、`cusnocn` 可以删（`DELETE rulesets/{id}`，有规则引用时 409，带 `confirm=true` 先把它从这些规则里去掉，只剩客户端条件或没有条件的规则一起删；删掉的记在 `webinfo/atom_builtin.json`）。只有一个下载来源的内置规则集可以改地址（`PUT rulesets/{id} {url}`，改完立即下载）；`adguard` 有多个来源，不能整体改地址（409）。内置规则集打开了 AtomDNS 原有的每日自动更新（`srs/*.json`、`adguard/config.json` 的 `auto_update`）；自动更新仍然不清缓存、不重算账本（4.5.1 的已知限制）。
- 和客户端规则的交互（已知）：客户端规则如果排在代理修正（或其他代理规则）后面，那些客户端查代理修正里的域名仍然会被判成代理。用法是把客户端规则放在最前面（4.2）。

### 4.5 规则变化后的生效

sing-box 的绑定只在收到 DNS 查询时更新，而客户端会缓存 DNS 应答，所以规则改动天然会有滞后。处理办法：

1. 清空 AtomDNS 的缓存。v0 直接全清，以后再细化为按受影响的域名清。**[定]** 第 2 步已实现：`/api/v1/atom/` 下每次成功的修改（规则、规则集）都清空配置里的所有缓存。DNS 页补全时（2026-11-12）收窄为只清确有必要的缓存，规则改动只清全局缓存，见 4.5.1 的"清缓存"。
2. **绑定账本**：AtomDNS 在内存里记录发给 sing-box 的每个域名最后一次被确认的指令，用 LRU 限制大小。规则变化后，用新规则重算这些域名，把有变化的 SET/CLEAR 直接用 DNS 查询发给 sing-box，不需要新接口。这样"组 X 改成组 Y"可以立即生效，客户端不用重新查询。**[定]** 第 5 步实现，细节见下面的 4.5.1。
3. 直连和代理互换的情况，客户端手里拿的是真实 IP，只能等它的 DNS 缓存过期。直连应答的 TTL 设上限 `direct_ttl_max`，默认 **300 秒**，可以通过 `PUT settings` 修改。**[定]**
4. **FakeIP 应答的 TTL** 设上限 `fakeip_ttl_max`，默认 **1 秒**，和生产环境的用法一致；也可以通过 `PUT settings` 修改。sing-box 给的是 600 秒（`C.DefaultDNSTTL`），太长了。**[定]** 有了 1 秒的 TTL，客户端很快会重新查询，新规则随之生效；账本主要用来照顾不遵守短 TTL 的客户端，比如 Java、浏览器自带的 DNS 缓存。

#### 4.5.1 实现（第 5 步）**[定]**

- **账本**（`rust/native-host/src/atom_ledger.rs`）：键是查询问题段里的原名字（wire 形式，保留大小写，sing-box 按原样保存绑定），值是 SET 某个标签或 CLEAR。容量固定 16384 条，不对用户开放，最久没有记录的先淘汰。账本是进程级的，AtomDNS 重新加载配置时不丢，进程重启后为空。
- **记录**：`ForwardCatalog` 发往 `nocnfake` 的 A 查询拿到 sing-box 的 NOERROR 应答后，按带的指令记一条（SET 组，或者 CLEAR）。AAAA 不记：sing-box 的 FakeIP 只有 IPv4 网段，它对 AAAA 直接返回空应答，根本不读指令，记了会让账本以为 sing-box 已经换了绑定（codex 第 1 轮）。账本自己发的指令被确认后也更新。SET 和 CLEAR 都记：原判断判成国外的域名也在账本里，用户给它加了规则以后，客户端手里的 FakeIP 立即跟着走。
- **重算**：对账本里的每个域名，用新规则表重新判断。客户端按"不属于任何客户端条件"处理，带 `client` 条件的规则一律不命中：这类规则只能是直连或屏蔽，命中它的查询不会发给 sing-box，所以 sing-box 里的全局绑定反映的是其他客户端得到的结论（4.2）。内置规则集的 fast_flags 按 mapper 现场算，和 `rules/test` 一样。新结论对应的指令：
  - 代理 → 组 Y：SET Y；
  - 直连：SET `direct`；屏蔽：SET `block`（生成的配置里都有这两个出站），客户端手里的 FakeIP 立即直连或被拒，直连时由 sing-box 用 resolver 解析域名；
  - 没有规则命中（交给原判断，没法离线算）：CLEAR，交给 Final，和"国外"的结论一致。
  
  和账本里的旧值一样就不发。
- **发送**：用 UDP 直接向 `singbox.dns_in`（127.0.0.1:6666）发 A 查询，带 65001 指令，格式和客户端查询经过 `edns_directive.rs` 后的一样。每次最多 32 个同时在途，单次等 1 秒，没回应就再试一次。
- **时机**：同步做。修改落盘、清空全局缓存之后，接口返回之前；进程里一把锁串行，后开始的重算一定用最新的规则表。接口返回时，sing-box 的绑定已经改好。
- **失败**：修改照样生效，接口照样返回成功，因为规则已经保存了。只有 NOERROR 应答才算确认；sing-box 的补丁在绑定保存失败（比如 cache file 写不进去）时让这次查询报错，不再照常应答（codex 第 2 轮，见 singbox-fakeip-outbound.md）。sing-box 没确认的域名，账本保留旧值，下次重算再试；客户端下一次查询也会带上新指令（代理规则的查询不经过缓存）。失败记日志，`GET status` 的 `bindings` 给出 `{size, capacity, last: {at, trigger, changed, failed, error?}}`。
- **各种改动之后**：
  - 规则的增、删、改、拖动、启用、停用，以及规则集的新建、修改、删除、`refresh`：都重算；
  - 带 `confirm=true` 删除节点组：引用它的规则改成 `final`，然后重算，绑定到这个组的域名发 SET final。从 sing-box 应用新配置到重算完成之间，大约几毫秒，绑定到已删组的连接会被阻断；
  - 节点组成员、Final 的成员和选中项：不用处理，绑定按标签找出站，selector 会自己跟着变；
  - IP 规则：不影响域名的结论；
  - 改 FakeIP 网段：sing-box 会清空所有映射和绑定，所以账本也清空；
  - sing-box 重启或重载：账本不变，绑定还在 `cache.db` 里（第 4 步验证过）。
- **清缓存**（2026-11-12，DNS 页补全时用户定的：只清确有必要的）**[定]**。缓存分两类：全局缓存（`cache_all`、`cache_all_noleak`，排在规则表后面，只有没命中规则的查询会查它，里面有 FakeIP 应答）和上游应答缓存（`cache_cn`、`cache_google`、`cache_google_node`、各上游组的 `cache_atom_<组>`，存的是上游的原始应答，和走哪条规则无关，没有 FakeIP）。
  - 规则的增、删、改、拖动、启用、停用，规则集的新建、修改、删除、手动刷新，重写改动：只清全局缓存。规则表和重写都在全局缓存前面，命中它们的查询本来就进不了全局缓存；要清的是加规则或改规则以前留下的旧条目，好让改动立即生效。
  - 规则集每日自动更新（4.9）：不清。旧条目被 lazy 命中时会在后台重跑整条流程，自己就纠正了；没过期的条目要等到过期。
  - 节点、节点组（包括带 `confirm=true` 删除组：引用它的代理规则改成 Final，代理规则的查询不进任何缓存，绑定由账本处理）、Final、mixed 端口、TTL 上限（在出口统一压）：不清。
  - 改 FakeIP 网段：清全局缓存（账本也清空，见上面）。上游应答缓存里没有 FakeIP。
  - `PUT dns`：改核心模式清全局缓存，requery 照旧启动（它按自己的 `flush_rules` 再清一遍）；ECS 不清（带 ECS 的缓存键里有 ECS，旧条目只是不再命中）；`prefer_v4`、`block_aaaa`、`block_qtypes`、`block_empty` 不清：它们都在全局缓存之前生效（`prefer_ipv4` 包住后面的整条流程，在外层压掉 AAAA 应答，缓存里存的是真实应答；其余三个在缓存之前直接应答）；关掉 `full_cache` 清全局缓存、关掉 `lazy_cache` 清它管的上游应答缓存（`cache_cn`、`cache_google`、`cache_atom_<组>`），释放内存；学习类别关掉只清那一类学到的条目。
  - 改上游组的上游列表（`PUT upstreams/{tag}`）：只清这个组自己的缓存：`domestic` 是 `cache_cn` 和 `cache_atom_domestic`，`foreign` 是 `cache_google` 和 `cache_atom_foreign`，`foreignecs` 是 `cache_google_node` 和 `cache_atom_foreignecs`，自定义组是 `cache_atom_<组>`。只改名不清。
  - requery 自己清缓存（`flush_rules` 加上所有 `cache_atom_<组>`）不变，那是它的用途。
  - **限制：发布新一代配置时全清**（2026-11-12 实测，主会话定为接受）。AtomDNS 发布新一代配置时，缓存只有"依赖指纹"（`config.rs` 的 `cache_dependency_fingerprints`）没变才沿用；依赖链里有 `domain_mapper`、`redirect`、`hosts`、`adguard_rule` 或带文件的 IP 匹配时一律不沿用。atom 自带配置里 8 个缓存都是这样，所以每发布一代，所有缓存都从空开始，转储文件也作废。这是 AtomDNS 的正确性保证，不改。会发布新一代配置的改动：`PUT dns` 的任何字段（核心模式、ECS、各开关、学习、缓存开关）、上游组的新建/修改/删除、改 mixed 端口（`main_proxy` 跟着改）。所以上面"ECS 不清""`prefer_v4` 等开关不清""上游组只清这个组的缓存"受此限制，实际全清；"关掉缓存开关清它管的缓存"照做（本来就清），`lazy_cache` 管 `cache_atom_<组>` 也是改生成的配置，同样随新一代全清。不受影响的是不发布新一代配置的改动：规则、规则集、重写、FakeIP 网段、节点、节点组、Final、TTL 上限、每日更新，以及设置页里单独清空一个缓存。
  - 清某个缓存失败时的处理照旧（4.7 的"清缓存失败"），接口返回 500，说明修改已经生效但缓存没清干净。
- **TTL 上限**：在 `execute_request`（UDP 和 TCP 的 53 端口共用）返回应答之前统一处理，缓存命中的应答也一样：A/AAAA 应答里只要有一个地址在 FakeIP 网段内，所有记录的 TTL 都压到 `fakeip_ttl_max`；否则压到 `direct_ttl_max`。只往下压，不往上提。没有地址的应答（空应答、NXDOMAIN、其他类型）不动。缓存里存的仍然是上游原来的 TTL，所以改上限后不用清缓存，下一次应答就按新值。两个设置存在 `atom_proxy.json` 的 `settings` 里，范围 1 到 86400 秒，不进 sing-box 配置，改了也不会触发重载。没有 `singbox` 段时（比如测试配置）不封顶。
- **已知限制**：
  - AtomDNS 进程重启后账本为空，之后改规则只影响重启后查询过的域名；不做持久化。
  - 被 LRU 淘汰的域名退回到原来的行为：等客户端重新查询。
  - 竞态：重算的同时，还在用旧规则表处理的查询可能比账本的指令晚到 sing-box，把绑定写回旧值。时间窗口就是一次查询的耗时。
  - 带客户端条件的规则加上之前，那个客户端已经拿到的 FakeIP，在过期前仍按全局绑定走（sing-box 不区分客户端）。
  - 不经过 atom API 的变化，比如 AdGuard、geosite 的在线更新，既不清缓存，也不触发重算。规则集的每日自动更新会重算，但不清缓存（见上面的"清缓存"）。
  - 重算只看规则表，不模拟排在它前面的系统固定项（重写/hosts、类型屏蔽，见 3.1）。如果某个记在账本里的域名后来被加了重写，之后再改规则时，账本照样按规则表给它发指令；而客户端重新查询时，会先命中固定项，根本到不了规则表。受影响的只是客户端手里那个旧 FakeIP 的出口，FakeIP 的 TTL 是 1 秒，客户端很快会重新查询并拿到重写的结果。（codex 第 1 轮，记为限制。）第 6 步把重写规则开放到了控制面（`rewrite`，第 8 节），重算仍然不模拟它，改它也不触发重算，这条限制保留。DDNS 第 6b 步成了规则，账本按规则表正常重算（直连 → SET direct）。

### 4.6 IP 规则

目标是 IP 的规则不经过 DNS，AtomDNS 处理不了。控制面把它们编译成 sing-box 的 `ip_cidr` 路由规则。这是唯一一类在 sing-box 里匹配的规则（2026-11-12 起还有端口、网络条件，见下）。**[建议]**

第 3 步定下的 **[定]**：

- 带 `ip_cidr` 的规则只能单独用，不能再带域名或 `client` 条件（否则 400）。动作可以是代理、直连、屏蔽。第 6b 步改为 **[定]**：IP 部分（`ip_cidr`、`ip_ruleset`）可以和域名条件写在同一条规则里，域名在 DNS 里匹配、网段在 sing-box 里匹配，用同一个动作；带 `client` 的规则仍然不能有 IP 部分（客户端名单在 AtomDNS，sing-box 判断不了）。`ip_ruleset` 的网段来自规则集下载内容里的 IP 条目（`<id>-<revision>.ip.txt`，规则集的 `ip_count`），按规则表顺序和 `ip_cidr` 合在一起编成 sing-box 路由规则；还没有网段的规则集不产生 sing-box 规则。规则的改动仍是先应用 sing-box 再保存；规则集下载改变了网段时，先保存规则集，再重新应用一次 sing-box（trigger `rulesets`），应用失败时规则集照样更新，sing-box 继续用旧网段（`in_sync` 为 false），下次应用时补上。
- AtomDNS 的 DNS 匹配跳过它们（`rules/test` 也不会命中）。启用的 IP 规则按规则表里的顺序编译成 sing-box 路由规则，排在 mixed 入站的 `resolve` 之后：代理 → `route` 到组，直连 → `route` 到 `direct`，屏蔽 → `reject`。网段写回规范形式（主机位清零）。
- 改动规则时，如果启用的 IP 规则（网段、动作、顺序）有变化，就走一遍 sing-box 应用事务（第 6 节），应用失败时规则的改动也不保存。只改域名规则不碰 sing-box。
- nft 部分（第 4 步实现，2026-11-12 改，见 9.4）：nft 把从 LAN 网卡进来、目标不是本机的 IPv4 TCP、UDP 全部导进 sing-box，不再按网段挑选，atomdns 也不再维护 nft 集合（`GET status` 去掉了 `nft`）。主路由只把 FakeIP 网段和要代理的网段转给这台机器，所以 IP 规则的网段要在主路由上转过来，否则只对 7890 有效。IPv6 的 IP 规则只对 7890 入站有效。
- 端口、网络条件和 FakeIP（2026-11-12）**[定]**：路由规则只管真实 IP 的连接。生成的每条规则都是 `logical and`：`{ip_cidr: [FakeIP 网段], invert: true}` 加上规则自己的条件（`ip_cidr`、`port`、`port_range`、`network`），作为保险把 FakeIP 网段排除在外（补丁已经让 FakeIP 连接在路由规则之前按绑定或 Final 走）。新增或改动的规则里 `ip_cidr` 条目和 FakeIP 网段重叠的拒绝保存（400），改 FakeIP 网段时和任何已有的 `ip_cidr` 条目重叠也拒绝；原来没有这项校验。只在写入时查，加载时不查（codex 复审后改）：以前存下的重叠规则照常加载（生成时反正排除了 FakeIP 网段），不会让程序起不来；而且只拒绝这次写入新加的重叠条目（2026-11-12 自查后改），所以那条旧规则还能改备注、拖动、启用停用、删掉它引用的规则集，改 FakeIP 网段时仍然检查所有条目。规则集里的网段（`ip_ruleset`）不校验，靠上面的排除。只有端口或网络条件、没有 IP 部分的规则匹配所有真实 IP；IP 部分来自规则集、但规则集还没有网段时，这条规则不产生 sing-box 规则（否则会放宽成所有 IP）；删除规则集后只剩端口或网络条件的规则一起删除。
- 没被任何规则命中的真实 IP 连接走 `route.final`（Final）：到达这台机器的转发流量都是主路由转过来、要代理的（9.1）。
- 没有配置 `singbox` 段时（比如测试配置），`ip_cidr` 规则仍然返回 400。

### 4.7 实现（第 2 步）

- **插件**：`atom_rules`（`rust/native-host/src/atom_rules.rs`，配置在 `atomdns/config/sub_config/atom_rules.yaml`）。它是一个 fixture，只给查询打动作标记：屏蔽 fast_mark 40、代理 41、直连 42；代理时另外把组写进 `ExecutionState::outbound`。主流程按标记分派，所以执行顺序仍写在 YAML 里。匹配复用 `matcher-core`：域名条件用 `MixSet`（full/domain 用排序池，regex、keyword 照旧），客户端用 `IpPrefixList`。规则逐条按顺序判断，第一条命中的生效。
- **位置**：重写、类型屏蔽、`unified_matcher1` 打标、IPv4 优先之后，原判断之前（第 6b 步前，DDNS 也在规则表前面）。原来排在前面的"记忆无 V4/V6"空答和黑名单屏蔽挪到规则表后面：否则一个被代理的域名如果曾经学到"无 V4"，会直接拿到空应答；直连规则也盖不过黑名单。刷新流程 `sequence_requery` 用同样的顺序。**[定]**
- **动作**：
  - 代理 → 组 X：A/AAAA 走 `$sequence_fakeip_exit`，向 sing-box 要 FakeIP，查询带 SET X，不问国内外 DNS；其他类型（MX、TXT 等）问国外 DNS，和灰名单在 `process_ot.yaml` 里的做法一样。AAAA 也发给 sing-box（同一个 SET），sing-box 只配了 IPv4 网段，返回空。注意（第 5 步 codex 复审时核对 sing-box 源码）：没开 IPv6 网段时，sing-box 在读指令之前就返回了空应答，所以 AAAA 带的 SET 实际不生效，只有 A 查询会改绑定。
  - 直连：A/AAAA 走 `$sequence_local_exit`（国内 DNS，真实 IP），其他类型走 `$sequence_local`；兼容模式和安全模式都问国内，因为这是用户明确指定的。
  - 屏蔽：`reject 0`，即 NOERROR 空应答。**[定]**
- **审计**：命中的规则写进 `domain_set`（`规则:<备注或 ID>`）和 `matched_rule_source`（`atom_rule:<ID>`）。
- **内置规则集**：YAML 的 `builtin` 列出规则能直接引用的名字：`adguard`（`adguard_rule`，按 AdGuard 自己的屏蔽/放行匹配）、`geosite_cn`、`geosite_no_cn`（`unified_matcher1` 打标的名单，按 mapper 给它们打的 fast_mark 判断，所以 `$atom_rules` 要排在 `$unified_matcher1` 后面）。
- **自定义规则集**：
  - 远程 URL：格式 text/YAML/JSON/SRS/`.dat`（要 `category`）/`.mrs`，下载后转换成本地列表；下载走 `main_proxy`（7890）。
  - 第 6b 步 **[定]**：域名和网段由文件本身决定。SRS 一遍读出两种；规则集用不上的条目（进程名、端口、网络类型等）按编码读过去并按种类计入 `skipped`，每条都占解码配额；读不完整（文件损坏，或有不支持读取的条目，如 AdGuard 规则）就报错，记进 `last_error`，不替换已保存的列表。`.mrs` 按文件头的 behavior 读域名或网段。`.dat` 的分类带 `reverse_match` 或 cidr 字段就按 geoip 读，否则按 geosite 读；GeoIP 反向匹配（哪怕分类是空的）一律报错"不支持 GeoIP 反向匹配"。
  - 本地列表：通过 API 编辑，条目是 `domain:`/`full:`/`keyword:`/`regexp:`，不带前缀的条目当作 `domain:`。
  - 第一次下载失败时，规则集照样创建，记下 `last_error`，内容为空。刷新失败时保留旧内容。
  - 被规则引用的规则集不能删除，返回 409 和这些规则的 ID。
- **持久化**：规则表存在 `webinfo/atom_rules.json`（`{version, rules}`），规则集存在 `webinfo/atom_rulesets.json`，都是写临时文件再改名。每次修改先校验、编译出新的匹配表，再写盘，最后整张表替换（`Live`）：查询要么看到旧表，要么看到新表，DNS 不中断。文件读不了或解析不了时，配置编译失败，程序不启动，不会悄悄换成默认表。规则表里有不合法的规则（比如带 `client` 的代理规则，见 4.2），也按文件损坏处理，停用的规则同样要校验。**[定]**
- **规则集列表文件**（codex 复审后改）：每个规则集有一个 `revision`，列表内容存在 `webinfo/atom_rulesets/<id>-<revision>.txt`。每次换列表（本地编辑、下载成功）都写一个新 revision 的文件，然后写 `atom_rulesets.json`，这一步是唯一的切换点。写元数据失败时删掉新文件，旧列表和旧元数据都还在，重启后还是旧的；成功后删掉旧文件。`revision` 为 0 表示还没有列表（远程规则集还没下载成功过，包括第一次下载失败），内容为空，也没有文件。`revision` 不为 0 而文件不存在时，按读不了处理，程序不启动。本地规则集新建时 `revision` 就是 1，所以加载时遇到本地规则集 `revision` 为 0，或者 `revision` 为 0 但 `rule_count` 不为 0，也当作状态不一致，程序不启动，不会悄悄变成空规则集。**[定]** 不为第 2 步早期没有 `revision`、文件名是 `<id>.txt` 的格式写迁移：那个格式没发布过，也没人在用；它的元数据缺 `revision`，会被上面的检查拒绝。 进程如果恰好在写完元数据、删旧文件之前退出，会留下一个没人引用的旧文件，不影响结果，目前不自动清理。
- **并发下载**：每次下载开始时领一个编号（只在内存里），并记下当时的来源（url、format、category）。下载完要写入前核对两样：编号还是这个规则集最新开始的那一次，来源也没被改掉。任何一样不对，就丢弃这次结果（成功和失败都不记录），返回 409。所以两次刷新重叠时，只有后开始的那次能写入，先开始、后结束的那次不会用旧内容覆盖新内容，也不会把成功状态改成 `last_error`。**[定]**
- **清缓存失败**：清空某个缓存时（清哪些见 4.5.1 的"清缓存"），如果写空的缓存文件失败（比如磁盘满），改为直接丢弃：先清内存，再删掉缓存文件，免得重启后又读回旧条目。丢弃也失败（文件删不掉）时，接口返回 500，说明修改已经保存并生效，但缓存没清干净。
- **group_id**（第 2 步）：校验格式：非空、没有首尾空白和控制字符、最长 64 字节。没有 `singbox` 段时只做这一项。
- **ip_cidr**：第 3 步实现，见 4.6。
- **group_id**（第 3 步）：配置了 `singbox` 段时，代理规则的 `group_id` 必须是存在的节点组或 `final`（Final），否则 400；加载时文件里引用了不存在的组同样按文件损坏处理。

### 4.8 规则的 DNS 上游（第 6b 步）**[定]**

用户定的：直连规则可以选一个上游组，不做"多选几个上游"；要一组特定的上游，就新建一个只放它们的上游组。代理规则的上游固定是 FakeIP，屏蔽规则不查上游。

- **规则字段**：`dns`（上游组的 tag：`domestic`、`foreign`、`foreignecs`，或自定义组）和 `cache`（默认 true）。只能用在直连规则上，其他动作带了返回 400；组不存在 400。不带 `dns` 且开缓存，就是原来的直连流程（国内组、`cache_cn`），不变；只是不再学习"无 V6"（2026-11-12 去掉的例外：规则命中的域名都不参与学习，学习结果只给原判断用。直连规则的 A/AAAA 改走 `sequence_rule_local_exit`，它和 `sequence_local_exit` 的区别只是不执行 `$learn_nov6_empty`）。
- **上游组**：YAML 里的 aliapi 插件（`nocnfake` 除外）加上自定义组。自定义组存在 AtomDNS 的 `webinfo/settings.json`（`atom_groups`：tag 和名字；上游列表在 `upstreams[tag]`），编译配置时生成同名的 aliapi 插件（`config/atom_routes.rs`），和内置组一样同时问全部启用的上游、取最先成功的应答，`use_proxy` 的条目经 `main_proxy`。tag 是 `u-` 加 8 位十六进制；名字最长 32 个字符。接口：`POST upstreams {name, upstreams}`（201，带新 tag）、`PUT upstreams/{tag}` 可以带 `name` 改名（内置组 400；自定义组不能给空列表）、`DELETE upstreams/{tag}`（内置组 400；被规则的 `dns` 引用时 409，带规则 ID）。这三个和 `PUT dns` 一样发布新一代 AtomDNS 配置，不占管理租约。
- **分派**：`atom_rules` 的 marks 加 `route`（自带配置 43）。编译时按上游组生成序列 `atom_direct`：第 i 个组有两个数字 mark，`70000+2i` 先过这个组自己的规则缓存 `cache_atom_<组>` 再问这个组，`70001+2i` 直接问；两种都先写审计标签（`flow_setter`），缓存命中也记得是哪个组；组和 mark 的对应表作为 `routes` 参数交给 `atom_rules`（YAML 里写 `routes` 会被拒绝）。规则命中时打 fast_mark 43 和对应的数字 mark，主流程和刷新流程在 fast_mark 42 之前加一支 `fast_mark 43 → [$atom_direct, exit]`。这些插件只存在于编译出的配置里，不写文件；所以加组、删组只是改设置，改规则永远不用重新发布配置。
- **缓存**：YAML 里的 `cache_atom` 只是模板，每个组复制一份（`cache_atom_<组>`，转储文件也各自一份），模板本身不编译。分开是因为带客户端条件的规则可以让同一个域名对不同客户端走不同的组，共用缓存会把一个组的应答给另一个组的客户端（codex 6b-2a 第 1 轮）。规则变化时不清它们（4.5.1 的"清缓存"）；改这个组的上游列表时清它。它们也归"上游应答缓存"开关（`lazy_cache`）管（2026-11-12）：生成的查缓存那一步带 `flag lazy_cache`，所以规则的 `cache` 实际是"规则开关且总开关"。`cache_google_node`（原判断里带 ECS 的国外查询缓存）不归它管，沿用 AtomDNS 原来的做法。`cache: false` 的查询不经过任何缓存（规则表在全局缓存之前，原判断的缓存也走不到）。
- **DDNS**：写成"直连 + 上游组 + 不缓存"的规则，放在规则表最前面，和原来"排在规则表之前的固定项"等价。默认规则表不带 DDNS 规则。原来的"国外失败回退国内"不再提供。
- **账本**：直连不管走哪个组，绑定都是 SET direct（4.5.1），不受影响。
- **和配置发布的并发**（codex 6b-2a 第 1 轮）：编译配置时读到的 `atom_rules.json`、`atom_rulesets.json`、`atom_proxy.json` 记进编译输入的指纹。发布新一代配置（`PUT dns`、上游组的改动）编译期间如果有规则或节点的写入落盘，提交时指纹对不上，这次发布返回 409，不会装上编译时读到的旧表；所以删组时的引用检查和随后的规则写入之间不会留下引用不存在的组的规则。
- **ECS 和刷新**（codex 6b-2a 第 2 轮）：`atom_rules` 的 `route_ecs`（自带配置 `{foreignecs: $client_ecs}`）让选了这个组的路由先加 ECS 再查，这个组的规则缓存按 ECS 区分，和原来的 `sequence_google_node` 一样。刷新：requery 的 flush 名单写不到生成的缓存，所以 requery 只要清缓存（`flush_rules` 不为空），就连同所有 `cache_atom_<组>` 一起清空，然后照常经 `atom_direct` 重新查询、重新填缓存（codex 第 3 轮；第 2 轮曾让刷新绕过规则缓存，结果刷新到的新应答不进缓存，客户端仍拿旧的）。ECS 是序列里单独的一步：`ecs` 只作用于它所在序列的后续步骤，放进内联块里就作用不到缓存和上游（codex 第 3 轮）。删组时连同它的缓存调优（`settings.caches.cache_atom_<组>`）一起删掉。
- **退役开关**：`settings.json` 里存着的 `ddns_foreign` 被忽略（下次写设置时去掉），不会让配置加载失败。

### 4.9 规则源目录和规则集的自动更新（第 6b 步）**[定]**

- **目录**：MetaCubeX 的 `meta-rules-dat`（`sing` 分支，`geo/geosite/<名>.srs` 是域名，`geo/geoip/<名>.srs` 是网段）和 blackmatrix7 官方的 `ios_rule_script`（`rule/Clash/<名>/<名>.list`，域名和网段在一个文件里）。条目 ID 是 `meta:<名>`、`bm7:<名>`。目录索引随配置带一份（`rule/atom_catalog.json`，2026-11-10 生成：geosite 2022、geoip 260、bm7 668），`POST catalog/sync` 和每日更新经 `main_proxy` 用 GitHub 的 trees 接口重新读，写到 `webinfo/atom_catalog.json`，比带的那份新就用它。`GET catalog?q=` 搜索。
- **托管规则集**：规则的 `ruleset`、`ip_ruleset` 引用目录条目时，如果还没有这个规则集，先建一个（ID 就是条目 ID，来源 `catalog`）并下载，下载失败返回 502，规则不保存；MetaCubeX 的两个文件合在一起。最后一条引用它的规则改掉或删掉后，它跟着删除。托管规则集不能手动新建或修改，只能刷新。下载里用不了的条目（IP-ASN、PROCESS-NAME 等）按种类计数（规则集的 `skipped`），界面提示数量。
- **每日更新**：配置开始服务 5 分钟后，然后每小时检查一次：远程规则集和托管规则集里一天没更新的重新下载，目录索引一天没同步的重新同步。每次下载持管理租约（和 API 写操作一样，不会撞上配置发布）；成功后重算账本（不清缓存，2026-11-12 改，见 4.5.1 的"清缓存"），网段变了按 4.6 重新应用 sing-box。新一代配置接管时旧的更新任务停止。

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
- **名称筛选**（2026-11-13）：组有一个可选的 `filter`，是对节点名称的正则（不区分大小写，单个关键字如 `美国` 也是合法正则）。名称命中的节点自动成为成员，之后新增、改名的节点按同一规则进出，不用改组。它和手动成员并存：`members` 只存手动选的，实际成员是手动成员加命中节点，生成配置、空组判断、selector 默认选中都按实际成员算。命中的节点在界面上锁定，不能逐个取消；接口返回里 `matched` 是当前命中的节点 ID。只从节点池里拉节点，不拉别的组，所以环检测不变。正则不合法或超过 200 字节时保存返回 400；节点改名导致 selector 选中的节点不再是成员时，选中回到第一个成员。Final 没有筛选。**[定]**
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
| `route.rules` | `dns-in` → hijack-dns；`mixed-in` → resolve（AtomDNS）→ FakeIP 地址走绑定或 Final，其他走 direct；IP、端口、网络规则（4.6）→ 对应的组，只对真实 IP 的连接生效。FakeIP 连接（三个入口）在路由规则之前按绑定或 Final 走（3.3） |
| `route.final` | Final selector |
| `experimental.cache_file` | 启用，`store_fakeip: true`（锁定） |
| `experimental.clash_api` | 只监听 127.0.0.1，带随机生成的密钥 |

- **节点域名的解析**不能经过 AtomDNS，否则国外节点域名会拿到 FakeIP，形成回环。`resolver` 默认用 AtomDNS 设置里的国内上游地址，由 sing-box 直接去问；用户也可以自己指定。**[定]**
- **FakeIP 网段 `28.0.0.0/8`**：这是已分配的公网地址段（美国国防部），公网上实际不路由，社区常用来做 FakeIP。代价是真要访问这个段的真实地址时会被当成 FakeIP。**[定]**
- **mixed 入站（7890）也按规则分流。** **[定]** 浏览器通过 7890 发来的是域名，sing-box 先用 `resolve` 动作（`server` 指向本机 AtomDNS 53 端口）问 AtomDNS，再按回答走：
  - 返回 FakeIP：按域名查绑定，有绑定走对应的组，没有走 Final，目标保留域名，由代理服务器远端解析；
  - 返回真实 IP：先过 IP、端口、网络规则（4.6），没命中走 direct；
  - 返回空或 NXDOMAIN：解析失败，连接被拒，等于屏蔽。
  
  AtomDNS 在返回应答之前已经把查询转给 sing-box 的 FakeIP 入口、写好了绑定，所以不存在时序问题。AtomDNS 不需要改动。直连、代理、屏蔽三种分流用原版的路由规则就能表达；"按组"这一步要在 sing-box 补丁里加一处：`matchRule` 里 resolve 完成后，如果解析出的地址属于 FakeIP，就按域名查绑定。估计 15 到 20 行。
  
  限制：每个新连接多一次本机 DNS 查询，只针对 7890 流量；AtomDNS 看到的查询来源都是 127.0.0.1，所以带"来源客户端"条件的规则对 7890 流量无效；7890 直接发来 IP 的连接，只匹配 IP、端口、网络规则，没命中就走 Final。
- **应用流程**：生成 → `sing-box check` → 原子写入 → 发 SIGHUP 让 sing-box 重新加载 → 通过 Clash API 确认新实例起来了；失败就写回旧配置再重载一次。sing-box v1.14.4 的 `cmd_run.go` 收到 SIGHUP 会重新加载配置。
- 切换节点和测速走 Clash API，不需要重载。

第 3 步定下和实现的 **[定]**：

- **进程管理**：sing-box 跑在独立的 systemd 单元 `atom-sing-box.service` 里（单元文件由安装器写，`ExecReload=kill -HUP $MAINPID`，`Restart=on-failure`）。atom 写完配置后执行 `systemctl reload-or-restart atom-sing-box.service`：单元在运行就发 SIGHUP，没在运行（第一次）就启动它。sing-box 崩了由 systemd 拉起，atom 重启不影响代理。
- **位置**：配置和 `cache.db` 放在 atom 配置根下的 `sing-box/`。程序路径、工作目录、重载命令、Clash API 地址（`127.0.0.1:9090`）、FakeIP DNS 入站（`127.0.0.1:6666`）、本机 AtomDNS 地址（`127.0.0.1:53`）、redirect 7892 / tproxy 7893、默认 resolver 来自哪个上游组（`domestic`）、nft 表名（`atom`，第 4 步加，见 9.4）写在自带 YAML 里 `atom_rules` 插件的 `singbox` 段，不对用户开放。
- **重载失败不退出**（参考 PR15 后定，sing-box 补丁）：上游 v1.14.4 收到 SIGHUP 后先关掉旧实例；新配置如果通过了 `check` 却启动失败（端口被占用这类运行期错误），进程会直接退出，要等 atom 回滚、systemd 重启，中间代理和国外 DNS 都断。补丁让 `cmd_run.go` 记住上一份成功运行过的配置，新实例启动失败时用它在内存里把旧实例重建出来，进程不退出。atom 这边不用改：旧实例不认新密钥，确认照样失败，atom 写回旧文件再重载一次，最后运行的还是旧配置。
- **确认新配置生效**：每次应用都生成新的随机 Clash API 密钥写进配置。旧实例不认新密钥，所以用新密钥在 20 秒内拿到 `/proxies`（里面有 `final`）就证明新配置已经在跑。代价：本机接了第三方 Clash 面板的话，每次应用后要重新填密钥（Clash API 只监听 127.0.0.1）。
- **选择要同步**（codex 复审后加）：sing-box 的 selector 启动时先用 `cache.db` 里记住的选择，再看 `default`。所以每次重载并确认后，atom 用 Clash API 把每个有成员的 selector 组和 Final 切到 atom 记录的选择；同步失败按应用失败处理（回滚）。回滚时旧实例也按旧状态重新同步一遍。
- **事务**：校验（规则表和节点状态）→ 生成 → 和运行中的文件比较，一样就跳过 → `sing-box check`（配置先写到 `config.json.next`；不通过返回 400，什么都不动）→ 改名为 `config.json` → 重载 → 确认 → 写 atom 的文件 → 生效。重载或确认失败：写回旧文件（之前没有就删掉），再重载一次并用旧密钥确认，接口返回 502，错误里写回滚结果；atom 的状态和文件都不变。sing-box 已经应用但写 atom 文件失败：同样回滚 sing-box，返回 500。一次应用进行时，其他写操作返回 409（不排队）；整个进程同一时间只有一次应用。这两层保护（进程级的应用锁和规则表持有者的 busy 标记）一直持有到 atom 的文件写完；写文件失败时，回滚 sing-box 也在保护之内完成，不会把别人刚应用成功的配置改回去（codex 复审后改）。找不到 sing-box 程序时不写任何文件，直接报错。
- **两个文件的写入顺序**（codex 复审后改）：只有确认删除组会同时改规则和节点状态。先写规则（引用改成 Final），再写去掉该组的 `atom_proxy.json`；两次写之间崩溃，留下的只是一个没被引用的组，下次照常启动。第二次写失败时把规则文件写回原样，写回也失败就在错误里说明。
- **凭据文件权限**：`webinfo/atom_proxy.json` 和 sing-box 的 `config.json`（含 `config.json.next` 和写入时的临时文件）都以 0600 写入。
- **启动时**（包括 AtomDNS 每次重新加载配置后）：生成一遍配置，和运行中的文件比较；不一致（第一次启动、升级后生成规则变了、默认 resolver 跟着国内上游变了）就应用一次；失败只记日志和 `status`，不影响 DNS。
- **生成的内容**（对应上表）：`inbounds` 的 redirect/tproxy/mixed 监听 `0.0.0.0`；`dns.servers` 是 `fakeip`、`resolver`、`atomdns`（mixed 入站 `resolve` 用），`dns.rules` 把 A/AAAA 交给 `fakeip`，`dns.final` 是 `resolver`；`route.default_domain_resolver` 是 `resolver`，节点域名不会走 FakeIP；路由规则依次是 `dns-in` → `hijack-dns`、`mixed-in` → `resolve`（`server: atomdns`、`disable_cache: true`）、IP/端口/网络规则（4.6，每条是排除 FakeIP 网段的 `logical and`）、`mixed-in` 且目标是域名（`domain_regex: ["."]`）→ `direct`；`route.final` 是 `final`；出口依次是节点、组、Final、`direct`、`block`。
- **resolver**：设置里留空时，用 `singbox.resolver_group` 那个上游组里第一个启用的 `udp://`/`tcp://` 地址条目，没有就用第一个带 `dial_addr` 的条目（`udp://dial_addr`），再没有用 `udp://223.5.5.5`。用户指定时只接受 IP、`udp://IP[:端口]`、`tcp://IP[:端口]`（域名本身还要解析）。
- **设置**（`settings`）：`fakeip_range`（IPv4 网段，前缀 8 到 24，主机位为 0，不能是回环；注意改网段会让 sing-box 清空已有的 FakeIP 映射和绑定，客户端缓存里的旧 FakeIP 失效，要等客户端重新查 DNS；这次重载如果失败，回滚到旧配置也找不回原来的映射，见 [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md) 的已知限制）、`resolver`、`mixed_port`（不能和 atom 的其他端口冲突）。改 FakeIP 网段后清空 AtomDNS 的缓存（旧网段的 FakeIP 应答）。改 mixed 端口后，如果 AtomDNS 的 `main_proxy` 正指向 `127.0.0.1:<旧端口>`，就把它改成新端口（写 `webinfo/settings.json` 并重新加载 AtomDNS 配置）；指向别的地址时不动。
- **7890 的 sing-box 补丁**：`resolve` 解析出的地址全部属于 FakeIP 时，按地址对应的域名查绑定，有就走绑定的组，没有就走 `route.final`；地址被丢掉，目标保留域名。见 [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md) 的"代理入站"。redirect、TProxy 入站还原 FakeIP 时同样如此（2026-11-12 起没有绑定也不再匹配路由规则，3.3）。

## 7. 锁定与开放

### 7.1 锁定（fork 里写死或删掉）

| 项目 | 处理 |
|---|---|
| `dns_mode` | 固定为 FakeIP，RealIP 的代码路径删除 |
| `cn_fakeip` | 固定为关 |
| FakeIP 上游（`nocnfake`） | 固定为本机 sing-box 的 `dns-in`（`udp://127.0.0.1:6666`）。`PUT upstreams/nocnfake` 拒绝修改它（第 6 步前是 `POST /api/v1/upstream/config`），`settings.json` 里存的列表也被忽略 |
| 国外 DNS 走的代理（`$main_proxy`） | 默认是 `socks5://127.0.0.1:7890`（`settings.yaml` 里写作 `addr: "127.0.0.1:7890"`），即本机 sing-box 的 mixed 入站，经 Final 出去。可以按上游单独关闭。UDP 上游不能走 SOCKS（AtomDNS 现有规则），所以默认的国外上游用 DoH/DoT/TCP。DoH over HTTP/3 和 DoQ 走 QUIC（UDP），同样不能走 SOCKS，选了它们的条目直接连上游（2026-11-12 合入上游 v0.6.6）。不用 nft 在 OUTPUT 链导流：那样要处理本机流量、同步上游 IP 集合，还要防回环。**[定]** |
| 专属分流组 | 删除。第 1 步在产品配置里删掉（不 include `special_groups.yaml`，主流程去掉 `$special_upstream_matcher`、`$sequence_special`）；第 6 步删掉 Rust 代码（`config/standard_groups.rs`、`standard_groups_api.rs`、编译和应用里的组状态、派生文件、孤儿清理和 health 的 `pending_cleanup`）。配置再 include `special_groups.yaml` 时按普通 include 处理（文件不存在就报错） |
| `realiplist`、`cnfakeipfilter` | 删除（"代理但返回真实 IP"在 FakeIP 网段分流架构下等于悄悄直连）**[定]**。核实过的语义：`realiplist`（mark 9）在所有名单里优先级最高，A/AAAA 和其他类型都问国外 DNS（经 `main_proxy`），返回国外真实 IP，流量不进 sing-box；`cnfakeipfilter`（mark 10）只在 `cn_fakeip` 开启时生效。两个名单、mapper 条目、名单文件、分流分支和只给 realiplist 用的 `sequence_google_exit` 都已删除。以后需要"国外 DNS 解析后直连"，在规则表里表达 |
| AtomDNS 原有的在线更新（程序、配置包、WebUI） | 删除，改为产品自己的更新渠道。第 1 步删掉了整个 `update` 模块（含 `update-guard` 子命令、中断恢复、`/api/v1/update/*`、`/api/v1/config/export`），容器模式（`ATOMDNS_CONTAINER`，用镜像里的配置包初始化卷）依赖配置包，一起删了；`docker/` 和发布脚本还没清理 |
| AtomDNS 原有的 WebUI 和规则名单 API | 第 6 步删除接口（6a）；`webui-log/` 前端工程在 6b 换成新 WebUI 时删除。删掉的 HTTP 接口：`/plugins/{tag}/...`（名单的 show/save/post、规则源的 config/rules/update、缓存的 flush/dump/load_dump、`requery`、`webinfo`、重写、域名学习、`domain_output`）、`/api/v1/special-groups`、`/api/v1/upstream/*`、`/api/v1/capabilities`、`/api/v1/settings/effective`、`/api/v1/flags*`、`/api/v1/cache/settings*`、`/api/v1/cache/inventory`、`/api/v1/learning/settings`、`/api/v1/overrides`、`/api/v1/domain-generation`、`/api/v1/system/webui-port`、`/api/v1/system/restart`（连同进程内重启），都返回 404。它们背后还被保留的功能用着的部分改走 atom 接口（7.2、第 8 节）。`requery` 定时任务在进程内调用缓存和学习名单的 save/flush，这些 `/plugins` 路由保留为进程内入口，不对 HTTP 开放。保留的原有接口：`/api/v1/system/health`、`/metrics`、审计（`/api/v1/audit/*`、`/api/v2/audit/*`）、诊断抓取（`/api/v1/capture/*`），都只读或只管诊断，不在 OpenAPI 规范里，不承诺稳定 |
| YAML 配置 | 由产品自带，用户不改。放在 fork 的 `config/`，来自 `jasonxtt/file` 的 `atomdns/config/full/config_rust`（v0.6.4，schema 7），改动见 `config/README.md` |
| sing-box `cache_file.store_fakeip` | 固定开 |
| 管理 API 监听地址 | 监听局域网；端口由安装器写入，默认 80（第 7 步）。面板登录可选（2026-11-14 加上，见第 8 节"面板登录"）**[定]** |

### 7.2 开放（通过控制面 API 修改）

`core_mode`（兼容/安全模式）、类型屏蔽、IPv4 优先、缓存开关和容量、国内外上游 DNS 列表、ECS、域名学习、FakeIP 网段（改了要连同 nft 规则一起重新生成）、直连应答和 FakeIP 应答的 TTL 上限（第 5 步实现：`PUT settings` 的 `direct_ttl_max`、`fakeip_ttl_max`，见 4.5）。

第 6 步定下的 **[定]**：AtomDNS 这一侧的开放项不进 `settings`，单独两个资源。原因：`settings` 写 `atom_proxy.json`、走 sing-box 应用事务；这些项写 `webinfo/settings.json`、发布 AtomDNS 的新一代配置，放在一个 PUT 里会一半成功一半失败。

- `GET/PUT dns`：`core_mode`、`block_qtypes`（SOA/PTR/HTTPS）、`block_aaaa`、`prefer_v4`、`full_cache`（界面叫"全局缓存"）、`lazy_cache`（"上游应答缓存"）、`block_empty`（空结果屏蔽，2026-11-12 开放）（只开放开关，缓存容量用 YAML 的默认值）、`ecs`（国外查询带的 ECS 地址，空串恢复默认）、`learning`（`enabled`、`remember_direct`、`remember_proxy`、`no_v4`、`no_v6`；关掉的学习名单随即清空）。改 `core_mode` 会启动一次 requery。
- `GET upstreams`、`PUT upstreams/{tag}`：国内（`domestic`）、国外（`foreign`）、国外带 ECS（`foreignecs`）三组上游，条目是 AtomDNS 原有的上游条目（`use_proxy` 就是 7.1 说的按上游关闭代理）；`protocol` 可以是 `udp`、`tcp`、`tls`/`dot`、`https`/`doh`、`h3`、`quic`/`doq`，DoH 加 `enable_http3: true` 就是 DoH over HTTP/3（和 `h3` 等价，界面上是 DoH 的 HTTP/3 开关，存成 `https` + `enable_http3`），`quic` 是 DoQ；这两种不能开 `use_proxy`（400）（2026-11-12 合入上游 v0.6.6）；FakeIP 组（`nocnfake`）不出现，也改不了。至少保留一个启用的条目；空列表恢复配置的默认列表。
- 这两个资源不带 `version`，后写的覆盖先写的。
- 其余开关不开放；`client_routing` 删除，改用规则表的客户端条件（4.2）。

### 7.3 跨组件的一致性

由控制面一处修改、同时下发到各组件：

- FakeIP 网段：sing-box 的 `inet4_range`、生成的路由规则里排除 FakeIP 网段的那一项（4.6）、主路由静态路由（文档提示用户配置）。nft 不再按网段挑选（9.4）。
- IPv6：v0 不做 IPv6 FakeIP；被代理域名的 AAAA 查询返回空应答。**[建议]**
- IP 规则网段：sing-box 路由规则；主路由要把这些网段转到这台机器（静态路由或走 LAN 那个口），否则只对 7890 有效。

## 8. 管理 API **[定]**

正式规范是 OpenAPI 3 文档 [`atomdns/docs/api/openapi.yaml`](../atomdns/docs/api/openapi.yaml)，WebUI 和第三方 UI 都按它实现。路径前缀 `/api/v1/atom/`。规范放在 atomdns 仓库、和代码一起提交（第 6 步定）：一致性测试要读它，而 atom 和 atomdns 是两个仓库。

**一致性**（第 6 步）：`atom_rules_api.rs` 的 `ROUTES` 列出全部路由（方法、路径模板）；测试 `the_openapi_document_matches_the_routes` 检查规范的 paths 和 `ROUTES` 完全一致，每条路由都能到达自己的处理函数（不落到兜底的 404/405），错误体都是 `{"error": string}`，成功的状态码在规范里写了，规范里每个路径没列的方法返回 405。规范本身用 `npx @redocly/cli lint` 校验。

| 资源 | 操作 |
|---|---|
| `rules` | 列表、新建、修改、删除、`POST rules/move`（拖动）、`POST rules/test`（输入域名，看会命中哪条规则）、`POST rules/dryrun`（一次查询的完整推演，见下） |
| `rulesets` | 列表、新建（远程 URL / 本地列表）、修改、刷新（远程和内置）、删除 |
| `nodes` | 列表、导入（`format: json|yaml|link`，可以批量）、修改、删除 |
| `groups` | 列表、新建、修改、删除（"默认代理"不可删）、`POST groups/{id}/select`、`POST groups/{id}/delay` |
| `final` | 读取、设置（成员和选中项，选中项只能是一个） |
| `settings` | 读取、修改 sing-box 一侧的设置和 TTL 上限 |
| `dns` | 读取、修改 AtomDNS 一侧的开放项（7.2，第 6 步） |
| `upstreams` | 列出上游组，整组替换一组的上游（7.2，第 6 步） |
| `rewrite` | 系统固定项：重写（hosts）规则，整份读取和替换（第 6 步；`ddns` 第 6b 步删除） |
| `caches`、`stats/upstreams`、`learning`、`requery` | DNS 一侧的现状（2026-11-12，DNS 页补全）：每个缓存的条目数、计数和容量，清空单个缓存；每个上游的请求数、出错、采纳和延迟；四类学习名单的条数，清空一类；定期刷新的开关、间隔、上次运行，手动触发 |
| `status` | 两个进程的版本、运行状态、最近一次应用 sing-box 配置的结果 |
| `apply` | 查询最近一次变更的应用结果（见下） |

- 写操作即时生效。每次写入是一个事务：校验 → 生成 → `sing-box check` → 写入 → 重载 → 提交；任何一步失败就整体回滚，返回明确的错误。
- `rules` 和 `rulesets`（第 2 步实现，`rust/native-host/src/atom_rules_api.rs`）：
  - `GET rules` 返回 `{version, rules}`。规则的 JSON：`{id, enabled, remark, match: {domain, suffix, keyword, regex, ruleset, client, ip_cidr}, action: {type: proxy, group_id} | {type: direct} | {type: block}}`。
  - `POST rules` 新建，可以带 `before_id`/`after_id` 指定位置，默认放最后。`PUT rules/{id}` 整条替换。`DELETE rules/{id}?version=N` 删除。
  - `POST rules/move {id, before_id | after_id, version}` 拖动，`version` 必填。其他写操作的 `version` 可选，带了就检查。版本不对时返回 409，错误体里带当前 `version`。每次规则变化，版本加 1。
  - `POST rules/test {domain, client?}` 返回命中的规则（`matched`、`index`、`action`、`group_id`、`rule`），没命中时返回 `{matched: false, fallthrough: "original"}`，即交给原判断。只看规则表，不模拟系统固定项和原判断。规则页原来用它测试域名，2026-11-12 改用 `rules/dryrun`；接口保留，界面不再调用（它算内置规则集标记时读学习结果，会给学到的条目记一次命中，没改）。
  - `POST rules/dryrun {domain, client?, qtype?: A|AAAA}`（2026-11-12，`rust/native-host/src/atom_dryrun.rs`）**[定]**：推演"现在重新判断一次会怎样"，不留痕迹。做法：用运行中这一代配置编译好的入口序列（`sequence_6666`）和真实的状态机跑一遍，匹配器、fixture（mapper、规则表、`fast_mark` 等）都是真的，顺序就是 YAML 的顺序；只替换主机侧对外部操作的处理：缓存一律跳过（不读不写，记下会经过哪些缓存）；`route_learn` 只算"会学什么"，不学；`top_domains` 不记；重写照插件的规矩（IP 目标同族给地址、否则空应答；域名目标经它的序列 `$sequence_local_in` 照样不留痕迹地解析，固定服务器的直接去问）；FakeIP 上游（sing-box）不问，不发 SET/CLEAR、不进绑定账本，按空 NOERROR 继续；其他上游照真实流程发出去（探测、直连的上游，ECS 一样，时限和前台查询一样），绕过缓存，用私有的尝试记录和私有的一份上游轮转计数（从当前值起，照常前进；真实查询的轮转不动），不进查询日志、统计和上游计数；状态机运行期间 route_learner 的查找是只读的（不记命中、不删过期条目，`route_learner::peeking`）。`prefer_ipv4` 照 `run_preference`：AAAA 时先看记住的证据，没有就再推演一次 A（这次的步骤不显示；真实流程两支并发、原分支结束后再等 500ms，推演里先后跑，A 那支限时"原分支的耗时 + 500ms"），A 有记录或会拿到 FakeIP 就是空应答，什么都不记住。规则表逐条的结果在规则表 fixture 运行的同一步里算，和它用的是同一张表。为了列步骤，`sequence-core` 的 `StepTrace` 加了一个默认空的 `rule(sequence, pc, matched)` 回调。
    - 返回：`steps`（重写、类型屏蔽、IPv6 屏蔽、AAAA 时的 IPv4 优先、规则表（到命中那条为止逐条的 `hit/miss/off/singbox`）、原判断各支（A 只列无 V4，AAAA 只列无 V6）、探测（上游、ECS、应答的 IP、"探测结果，未学习"、`would_learn`）），每步 `hit/miss` 和简短原因（第一个不成立的条件：开关关、查询类型不符、不在名单、没有记录）；`branch`：决定结果的原判断分支（和 `original.ts` 的 key 一致）；`conclusion`：`direct`（上游组、应答的上游、ECS、地址、会经过的缓存）、`proxy`（`SET 组` 或 `CLEAR`，AAAA 时 sing-box 不读指令，不对照绑定；用 Clash API 的 `now` 把出口展开到节点，`chain`）、`block`、`empty`（rcode），以及上游失败时的 `fail`；`binding`：绑定账本里这个域名的记录、这次判断会发的指令（直连、屏蔽规则按账本重算的规矩是 SET direct / SET block，带客户端条件的规则不算；原判断的直连和空应答不发；账本里没有记录时，只有代理到组才算不一致），不一致时 `match: false`。
    - 和真实流程的出入（已知）：探测和直连查询每次都真的问上游，和真实查询可能拿到不同的应答（上游本身的变化、多个上游里先答的那个）；主机名上游（地址写域名、没有 `dial_addr`）的引导解析结果和到上游的连接，推演和真实查询共用，不隔离（2026-11-13，用户定，记为已知限制）：共用的是上游服务器本身的地址和连接，不是被测域名的 DNS 应答，不影响判断结果。不支持 `fallback`（atom 的配置里没有），遇到就报错。
    - 一致性测试 `atom_dryrun_tests.rs`：用仓库里实际的 `config_custom.yaml`、`process_*.yaml`、`forward_1.yaml`、`route_learner.yaml`、`flags.yaml`、`cache.yaml`，数据集合和上游换成本机假服务，对规则代理、规则直连（含指定上游组、不缓存）、客户端规则、geosite 国内外、探测（国内 IP、`direct_ip`、国外 IP、无 IP）、学到以后的记忆代理/直连、AAAA、重写、安全模式（ECS）、IPv6 屏蔽逐个先推演再真实查询，结论和真实结果（地址、sing-box 收到的指令）一致；推演前后缓存条目数、学习条目和命中数、热门域名、绑定账本、sing-box 收到的查询都不变。
    - 规则页的测试框：域名、客户端 IP（可选）、A/AAAA；结果是结论卡片（直连/代理/屏蔽/空应答、上游或出口链"兜底分组 → 默认代理 → 节点"、缓存、绑定，不一致时标"和推演不一致 · 客户端还没重新查询"）加步骤列表（规则表只展开命中那条，其余折叠）；命中规则时标亮规则行，交给原判断时自动展开底部原判断一行并标亮对应分支（第 11 节第 10 条）。
  - `GET rulesets` 列出内置集合（`type: builtin`）和自定义规则集；`GET rulesets/{id}` 带上列表内容。`POST rulesets` 的请求体是 `{name, type: remote, url, format?, category?}` 或 `{name, type: local, rules}`。`PUT rulesets/{id}` 改名、改地址或替换本地列表。`DELETE rulesets/{id}` 删除。`POST rulesets/{id}/refresh` 重新下载。
  - 错误体都是 AtomDNS 现有的 `{"error": "..."}`。写操作和 `rulesets` 的下载都占用管理租约，和设置、配置重载互斥（租约只防配置重载，不让写操作排队）。
  - 带 `client` 的代理规则返回 400（4.2）。下载期间规则集的来源被改掉，或者之后又开始了一次下载，返回 409，这次结果丢弃。修改已生效但缓存没清干净时返回 500（4.7）。
- 节点、组、Final、设置、`status`、`apply`（第 3 步实现，`rust/native-host/src/atom_proxy_api.rs`）：
  - `GET nodes` 返回 `{version, nodes}`，每个节点带 `id`、`name`、`type`、`server`、`server_port`、去掉凭据的 `outbound`（5.1）和所在的组 `groups`。`POST nodes {format, content}` 导入，`content` 是字符串（`json` 格式也可以直接给 JSON 值），返回 201 和新节点。`PUT nodes/{id} {name?, format?, content?}`（`format` 和 `content` 要一起给，整体替换连接），`DELETE nodes/{id}?version=N`，`POST nodes/{id}/delay {url?, timeout?}` 返回 `{id, delay}`。
  - `GET groups` 的每个组带 `empty`、`builtin`，selector 带当前选中 `now`。`POST groups {name, type: selector|urltest, members, selected?, url?, interval?, tolerance?}` 新建，`PUT groups/{id}` 整组替换，`DELETE groups/{id}?confirm=true`（见 5.3），`POST groups/{id}/select {name}`，`POST groups/{id}/delay {url?, timeout?}`（毫秒，默认 5000）返回 `{delays: {成员: 毫秒}}`。
  - `GET final` 返回 `{version, members, selected, empty}`。`PUT final {members?, selected?}`：成员变了走应用事务；只改选中项走 Clash API。
  - `GET settings` 返回 `{version, fakeip_range, resolver, resolver_effective, mixed_port, direct_ttl_max, fakeip_ttl_max}`，`PUT settings` 改其中任意几项（TTL 上限是第 5 步加的，见 4.5）。
  - `GET status`：`atomdns.version`；`singbox.running`、`version`（Clash API 的 `/version`）、`in_sync`（运行中的文件是否就是当前状态生成的）、`host`；`last_apply`；`bindings`（绑定账本，第 5 步，见 4.5.1）；`warnings.empty_groups`。原来的 `nft`（集合的网段、表名、最近一次更新的结果）在 2026-11-12 nft 改成全部导入时删掉（9.4）。`GET apply` 返回 `last_apply` 和 `in_sync`，`POST apply` 用新密钥重新应用一次。`last_apply` 是 `{at, trigger, ok, changed, error?}`，`trigger` 是 `nodes`、`groups`、`final`、`settings`、`rules`、`apply`、`startup` 之一，在进程内跨配置重载保留。
  - 写操作都可以带 `version`（请求体，DELETE 用 `?version=`），和节点状态的版本比较。sing-box check 不通过 400，重载或确认失败 502（已回滚），一次应用进行中 409。
  - 写操作占用管理租约，`PUT settings` 例外：它自己拿租约，只覆盖 sing-box 的部分，因为改 mixed 端口要重新加载 AtomDNS 配置，而重新加载要等所有租约结束。
- `dns`、`upstreams`、`rewrite`（`rust/native-host/src/atom_dns_api.rs`，第 6 步；`ddns` 第 6b 步删除，上游组的新建、改名、删除第 6b 步加上，见 4.8）：
  - `GET dns` / `PUT dns` 见 7.2；返回改后的全部字段。`GET upstreams` 返回 `{groups: [{tag, customized, upstreams}]}`，`PUT upstreams/{tag} {upstreams}` 返回同样的结构。这两个写操作不占管理租约（理由同 `PUT settings`：发布新一代配置要等所有租约结束），没有 `version`；发布期间别的请求可能拿到 503，重试即可。已知限制（第 6 步联调发现，AtomDNS 原有行为）：新装时 AdGuard 规则文件还没下载下来，加了节点以后后台下载才成功；下载如果正好落在新一代配置编译和提交之间，提交时发现输入文件变了，返回 409（`managed input changed externally`），什么都没改，重试一次即可。
  - （`ddns` 的内容是第 6 步时的记录，第 6b 步已删除。）`GET ddns` / `GET rewrite` 返回 `{entries}`；`PUT` 同样的 `{entries}` 整份替换：先校验，再写文件，最后生效（重写原来是先改内存再写文件，写失败时内存和文件不一致，改掉了），然后清空所有缓存。DDNS 条目和名单文件同格式；重写一行是"域名规则 + IP 或目标域名"。（2026-11-12 起 `PUT rewrite` 只清全局缓存，见 4.5.1。）codex 第 1 轮复审后补的：`ddns` 要等名单和用到它的 mapper 都重建完才返回（重建正在进行时只会被标记为再建一次，原来会提前返回）；重写文件记进配置编译的输入指纹，`PUT dns` 等发布新一代配置时，如果重写文件在编译之后被改过，提交时返回 409，不会把旧的重写规则装回去。重写插件配置了多个文件时 `PUT rewrite` 返回 409，不整份替换（只写第一个文件的话，其余文件的规则下次加载会回来；codex 第 2 轮）。atom 自带的配置只有一个文件。`PUT ddns` 同样只在名单恰好来自一个 `.txt` 文件、没有内联 `exps` 时才能整份替换，否则 409；单条 64 KiB 以上（加载器的上限）400；写进文件但重建失败（仍在用旧规则）时 500，不再报成功（codex 第 3 轮）。上游条目的 `protocol` 在规范里是必填，和代码一致，有测试按规范的必填字段核对。
  - 重算账本不模拟这两项（4.5.1 已知限制），改它们不触发重算。
- 内置规则集：`GET rulesets` 里是 `{id, name, type: builtin, refreshable}`；`POST rulesets/{id}/refresh` 返回 `{id, sources, updated}`，一个来源都没下载成功时 502、列表不变。同一个来源正在下载（启动时的下载、另一次刷新）时先等它结束，再下载一次（codex 第 1 轮）。
- 错误格式沿用 AtomDNS 现有的格式。**[定]**
- **面板登录**（2026-11-14，用户定：登录页 + Cookie，安装器里可以留空，设置页可以改）**[定]**，代码 `rust/native-host/src/panel_auth.rs`：
  - 凭据在 `webinfo/atom_auth.json`（0600）：用户名、密码的 PBKDF2-HMAC-SHA256 哈希（10 万次，随机盐，在阻塞线程里算）、一个随机密钥。没有这个文件就不需要登录，和原来一样。
  - 有文件时，除了 UI 文件、`/api/v1/system/health` 和 `auth`、`auth/login` 以外的请求（atom API、`/metrics`、审计、`/plugins`、诊断抓取）都要带会话 Cookie `atom_session`（登录时发，30 天，HttpOnly、SameSite=Strict，内容是"过期时间 + 用密钥算的 HMAC"），或 `Authorization: Bearer <密钥>`（安装器以 root 读这个文件拿密钥，升级、保留数据重装时调接口用），否则 401。
  - 接口：`GET auth`（`{enabled, authenticated, username}`）；`PUT auth {username, password, current_password?}` 设置或修改，换新密钥（所有会话失效），给当前浏览器发新 Cookie；`DELETE auth {current_password?}` 关闭登录；`POST auth/login`（错误时等 1 秒再返回 401）；`POST auth/logout` 清掉 Cookie。已开登录、用会话改的时候要给当前密码。
  - WebUI：未登录时整个界面换成登录页；顶栏有退出按钮；设置页"面板登录"可以设置、修改、关闭。安装器：网络那一步面板端口下面有可选的用户名和密码，装完最后一步设置。
  - 已知限制：退出只清本浏览器的 Cookie，那个会话本身到过期前仍然有效（改密码会让所有会话失效）；登录失败只有 1 秒延迟，没有锁定；面板是 HTTP，局域网里能抓包的人能拿到 Cookie。

## 9. 部署

### 9.1 形态 **[定]**

DNS 和代理在同一台 VM 上。客户端的 DNS 指向这台 VM；FakeIP 网段的流量想办法送到这台 VM。

| 拓扑 | 流量怎么进来 | 说明 |
|---|---|---|
| 单网卡旁路由（和主路由在同一网段） | 主路由加静态路由：FakeIP 网段和要代理的网段 → VM | 普通用户最常见的情况 |
| 双网卡网关（LAN/WAN） | VM 的 LAN 接主路由（用户的接法是主路由的 WAN2），主路由把 FakeIP 网段和要代理的网段走这个口 | 安装器提示：主路由需要把 FakeIP 网段和要代理的网段转到这台机器。TCP、UDP 进 sing-box，其余（ping 等）NAT 后从 WAN 出去。**[定]** |

用户的部署（2026-11-12）：atom 在主路由后面，主路由只把 FakeIP 网段和需要代理的网段转给 atom，所以到达 atom 的转发流量都是要代理的。VM 上：TCP 用 redirect，UDP 用 TProxy，从 LAN 网卡进来、目标不是本机的 IPv4 TCP、UDP 全部导进 sing-box（两种拓扑一样，原来只导 FakeIP 网段和 IP 规则网段）；sing-box 里 FakeIP 按绑定或 Final 走，真实 IP 按 IP、端口、网络规则走，没命中走 Final。sing-box 自己发出去的流量走 `output`，不会被再导回来。安装器检测网卡数量，生成对应的 nft 规则。具体规则写在单独的部署文档里。

### 9.2 安装器（第 7 步）**[定]**

一条命令装好，其余在浏览器里做。代码在 `installer/`，说明见 [installer/README.md](../installer/README.md)。

- **三层**：用户粘贴 `curl -fsSL <地址>/install.sh | sudo bash`（Alpine：`apk add bash curl && curl -fsSL <地址>/install.sh | bash`）；脚本装好 `atom-installer`（Go 标准库写的单个静态程序，向导页面内嵌）并打印"请在浏览器打开 http://<IP>:<端口>/"；浏览器里配好 DNS 和 sing-box 的基础设置，确定后自动跳转到面板。装完安装器自己退出。
- **下载**（脚本）：配套包 `atom-<版本>-linux-{amd64,arm64,amd64-musl,arm64-musl}.tar.gz`（脚本按架构和系统选一个，见 9.5；每个包：atomdns、补丁版 sing-box、`atom-installer`、`atom-webui.tar.gz`、默认配置、`manifest.json`），来源按顺序试：GitHub Release 直连，然后加速前缀（`gh-proxy.com`、`ghfast.top`、`down.mxw.xx.kg`、`gh.noki.icu`，可用 `ATOM_MIRRORS` 改）。连接 6 秒没通，或速度低于 50 KB/s 持续 10 秒，就换下一个。四个包的 sha256 在发版时写进 `install.sh`，任何来源下载的都要校验，不过就丢弃换下一个（加速源是第三方）。`ATOM_PKG` 可以指定本地包或地址（开发和离线用）。第 7 步没有发布 Release：下载路径用本地模拟的"GitHub 不通、慢镜像、好镜像"验证过，没有真实访问过 GitHub 上的包；公共加速源的可用性会变（2026-11-11 探测：`gh-proxy.com`、`ghfast.top` 通，`ghproxy.net` 超时，`mirror.ghproxy.com` 连不上），上线前要再核。2026-11-13 在阿里云广州（不走代理）用 38MB 的 v0.1.0 包实测（每个源最多 40 秒，核对 sha256）：直连只有 43 KB/s；`gh-proxy.com` 4.5 MB/s、`ghfast.top` 5.9 MB/s；`ghproxy.net` 90 KB/s，`ghproxy.homeboyc.cn` 403，`toolwa.com` 402（部署停用），这三个不用；`github.akams.cn` 是节点列表页，从它列出的 67 个公益节点里选了较快的 `down.mxw.xx.kg`（10 MB/s）、`gh.noki.icu`（7.9 MB/s）放在后面（个人节点，可能失效，靠 sha256 兜底）。
- **向导**（`web/index.html`，只放标签、错误和两处必须的提示：网关形态的警告，完成页的两步：主路由加静态路由 FakeIP 网段 → LAN IP、客户端 DNS 设为 LAN IP，两种拓扑一样）：
  1. 环境：系统（Debian 12+、Ubuntu 22.04+ 用 systemd，Alpine 3.19+ 用 OpenRC；x86_64 或 aarch64；root；Docker/LXC 容器里不支持）；53 端口被占时列出占用者并给按钮：systemd-resolved 的 stub 写 drop-in 关掉并把 `/etc/resolv.conf` 指到上游列表，其他 systemd 服务停止并禁用，OpenRC 服务停止并从运行级别删掉（见 9.5），不是服务的只提示；做完显示"已处理完毕"。
  2. 网络：单网卡旁路由或双网卡网关，选网卡；选网关必须勾选确认（警告 2026-11-12 改为"主路由需要把 FakeIP 网段和要代理的网段转到这台机器"）；面板端口，默认 **80**；面板用户名和密码（2026-11-14 加，可选：留空就不需要登录，保留数据重装时留空就不改；见第 8 节"面板登录"）。
     - 双网卡网关另有"LAN 侧网关"和"客户端网段"（2026-11-12，用户在 atom 测试机上发现）：客户端不在 LAN 网段、经 LAN 侧的路由器过来时，本机的应答（DNS、redirect 和 TProxy 的回包、NAT 回来的包）会沿默认路由从 WAN 出去。向导按本机已有的、经 LAN 网卡的路由预填这两项；安装器在 `policy-route.sh` 里对每个网段写 `ip route replace <网段> via <LAN 侧网关> dev <LAN 网卡>`，随 `atom-dns.service` 每次启动加回。没用连接标记改道：atomdns 的 UDP 监听在通配地址、按路由选源地址，改道改不了已经选成 WAN 地址的源，DNS 仍然不通。都留空表示客户端都在 LAN 网段。
     - 单网卡旁路由可以勾"回包经主路由"（2026-11-14，用户定），填主路由地址（默认是 LAN 网卡默认路由的网关）：导入 sing-box 的连接打连接标记，回包按标记经表 234 交给主路由，主路由能看到两个方向的包；同时关掉 LAN 网卡接收 ICMP 重定向。默认不勾。规则见 deploy-nft.md 3.4。
     - 所有权：网段原来就有同一网关、同一网卡的路由，就算机器自己的，卸载不删；网关不同就拒绝安装。安装器加的记在 `changes.json` 的 `routes`，拆网络时只删这些；重装时不再需要的旧路由当场删掉。
     - 面板只给 LAN（2026-11-12，用户定）：双网卡网关的 `atom-gateway.nft` 在 WAN 网卡上丢掉发往面板端口的 TCP；atomdns 仍监听所有地址，安装器和升级经 127.0.0.1 调用接口，LAN 地址变了也不受影响。完成页把浏览器带到 LAN 地址。实测（atom 测试机，0.1.3 → 0.1.4 升级）：从 LAN 打开面板 200、从 WAN 被拦，经 WAN 的 SSH 不受影响，客户端路由随升级保留。没处理：DNS 53 和 mixed 代理端口在 WAN 上照样开着。
  3. DNS：国内、国外上游（默认值是产品配置里的，只在和现在的不同时才写）。
  4. sing-box：FakeIP 网段、mixed 端口、可选导入节点（放进"默认代理"组，否则装完国外流量全被拒绝）。
  5. 确认并安装：实时进度，装完自检，倒计时跳转面板。
- **访问控制 [定]**：安装器不要令牌；默认端口 9996，被占就由系统另选一个空闲端口。只接受私网和回环来源的请求，Host 只认 IP、localhost 和本机名，写操作必须是同源的 JSON；闲置 30 分钟自动退出，装完 5 分钟内无人使用也退出。向导只有一个关闭入口（`/api/finish`、闲置计时器、引导脚本都走它）：有安装在跑就拒绝，否则标记 closing，此后不再接受安装和"停用"，然后退出；引导脚本只在收到 200 并且向导单元退出之后才替换包，超时或其他回应一律拒绝并提示手动处理。
- **装到哪里 [定]**：全部在 `/opt/atom/`：`bin/`（atomdns、sing-box、atom-installer）、`etc/`（nft 规则、`install.json`、`changes.json`）、`lib/policy-route.sh`、`data/`（配置根，atomdns 的 `-d`）、`backup/`。`/opt/atom` 之外只有两个 systemd 单元 `atom-dns.service`、`atom-sing-box.service`，以及用"停用系统 DNS 占用"时写的 resolved drop-in 和 `/etc/resolv.conf` 的链接；OpenRC 上是 `/etc/init.d/atom-dns`、`atom-sing-box`，日志在 `/var/log/atom/`，另有 `lib/reload-sing-box.sh`，停用 53 端口的服务后 resolv.conf 若被改写，原内容在 `etc/resolv.conf.orig`；`ip_forward` 由 `atom-dns.service`（OpenRC：`start_pre`）设置，不写 `/etc/sysctl.d`。`changes.json` 记录安装器改过的系统设置（resolved、停用的服务、`ip_forward` 原值、装过的软件包），卸载按它还原。产品配置里 sing-box 的路径是 `/opt/atom/bin/sing-box`。
- **安装步骤**：依赖（缺 nftables、iproute2 就用 apt 装，Alpine 用 apk）→ 程序 → 配置（`data/` 和 `ui/main/`，面板端口写进 `config_custom.yaml` 的 `api.http`）→ 网络（`nft -c` 校验后写 nft 规则、策略路由脚本和两个单元；`atom-dns.service` 的 `ExecStartPre` 依次加载 `atom.nft`、网关形态再加 `atom-gateway.nft` 和 `sysctl -w net.ipv4.ip_forward=1`、`policy-route.sh`，见 deploy-nft.md 第 4 节）→ 启动（有上次留下的 sing-box 配置就先重启 sing-box，再重启 atomdns）→ 设置（调用 atomdns 的 API；失败只警告，面板里可以改；填了面板登录的话最后设置它，之后安装器的请求都带 `webinfo/atom_auth.json` 里的密钥）→ 自检（两个服务、端口、`status` 里 sing-box 与配置一致，`www.google.com` 得到 FakeIP、`www.baidu.com` 得到真实 IP；DNS 自检只警告）。
- **失败和所有权**：`changes.json` 里的 `network` 在安装器开始改网络（写 nft 规则、单元、`ip_forward`、策略路由）时置位，撤销或拆除后清除；只有置位时才拆网络（停服务、删 nft 表、`ip rule`、清空表 233、恢复 `ip_forward`），所以回滚过的安装、或别人后来用了表 233，卸载都不会去动。表 233 已被别的配置使用时，安装在改任何东西之前就被拒绝。任何一步失败，撤销网络部分，文件和已做的 53 端口处理保留，页面显示错误和日志，可以返回修改或重试。
- **重复运行和数据**：向导完成时在 `data/.atom-data` 留一个标记。有标记（卸载时保留了数据）就沿用里面的设置、规则和节点，向导只问环境和网络，DNS 与 sing-box 两步不出现；没有标记（第一次，或上一次没装完）时，向导的值和 atomdns 现在的值比较，不同才写。
- **升级**：再粘贴同一条命令。已装好（`install.json` 里 `complete`）就由新包里的 `atom-installer upgrade` 做：同版本什么都不做，更旧的拒绝（`--force` 可以）；先备份程序、nft 规则、单元和自带配置到 `backup/`（留最近 3 份），再换程序、覆盖自带配置（`config_custom.yaml`、`sub_config/`、`rule/atom_catalog.json`、`ui/main/`），保留 `webinfo/`、每日更新下载的规则和缓存，重新生成 nft 规则和单元，重启 atom-dns（sing-box 程序变了才重启它）；atomdns 的接口起不来或 sing-box 没有处于最新配置，就恢复备份并等服务恢复。
- **卸载**：`/opt/atom/bin/atom-installer uninstall [--purge] [--yes]`：`install.json` 和 `changes.json` 有任一个就能运行（向导里点过"停用"、但安装在写 `install.json` 之前失败时，也能还原那个服务）。按记录还原：网络部分只在 `network` 置位时拆；单元文件只在有 `install.json` 时删；还原 resolved 和被停用的服务（回到原来的启用和运行状态），删 `/opt/atom` 下除 `data/` 外的全部文件；`--purge` 连数据一起删。装过的软件包（nftables）不卸载；OpenRC 上 `/var/log/atom` 不论是否 `--purge` 都删。
- **验证（第 7 步，2026-11-12，测试机 `ssh 77`：干净的 Debian 13，2 核 2G，没有 nft，systemd-resolved 占着 53）**：
  - 用 `ATOM_PKG` 加两个假镜像（一个限速、一个正常，GitHub 地址指向不存在的仓库）跑 `install.sh`：慢镜像被丢弃，好镜像下载、sha256 校验通过，起向导。
  - 浏览器里走完整个向导（环境里点"停用系统 DNS 占用"→ 已处理完毕；单网卡旁路由，网卡是接 netns 客户端的 veth，改了国内上游和 mixed 端口，导入一个 socks 节点；`apt` 装上 nftables），自动跳转到 80 端口的面板，概览页正常。
  - 客户端（netns）：`www.google.com` 得到 FakeIP，TCP 经 nft redirect 进 sing-box、从节点出去（节点那一侧的 sing-box 日志看到连接），UDP（NTP）经 TProxy 进 sing-box，mixed 端口走 socks5h 通；`www.baidu.com` 得到真实 IP。
  - 双网卡网关（面板端口 8080）：`ip_forward` 变成 1，`atom_nat` 表在；客户端默认网关指向本机，FakeIP 流量进 sing-box，其他流量经 NAT 转发出去而不经过 sing-box；卸载后 `ip_forward` 回到 0、nft 表和 `ip rule` 都没了。
  - 同版本再运行：提示"已经是 0.1.0"；升级到 0.1.1：规则（一条自己加的规则）保留，两个服务正常；升级到一个起不来的 0.1.2：恢复备份，服务和代理照常。
  - 卸载保留数据后重装（沿用规则和设置，向导跳过 DNS 和 sing-box 步骤）、`--purge`、装到一半失败（atomdns 起不来）时撤销网络部分，resolved 的还原，安装器端口被占时随机选端口，都验证过。
  - 第 3 轮 codex 之后的回归（`/api/finish` 的唯一关闭入口和卸载的所有权记录）：向导进程被冻结（SIGSTOP）时，引导脚本收不到同意，拒绝替换包，包文件原样；向导里停用了系统 DNS、安装在写 `install.json` 之前失败（包里没有 sing-box）时，卸载还原了 stub、drop-in 和 `/etc/resolv.conf`，没动网络；安装失败回滚后，有人往表 233 放一条路由，卸载不碰它；安装进行中，引导脚本、第二次安装、"停用"、finish 都被拒绝。
  - 没验证：从 GitHub 实际下载（没有 Release）、真实主路由下的单网卡形态（第 4 步已说明）、Ubuntu、公共加速源在国内网络下的表现。

### 9.3 不处理的绕过路径

浏览器开启 DoH、IPv6 RA 下发了别的 DNS，这类客户端拿不到 FakeIP。客户端的网关是主路由，旁路 VM 屏蔽这些也没用，所以不处理。**[定]**

v0 不做 IPv6 FakeIP，被代理的域名只返回 IPv4。

### 9.4 nft 和策略路由（第 4 步）**[定]**

具体规则、端口、标记和实测环境见 [deploy-nft.md](deploy-nft.md)。

- **规则**（2026-11-12 改）：nft 表 `inet atom`，`prerouting` 上只看局域网网卡进来、目标不是本机地址（也不是广播、组播）的 IPv4 包：TCP 全部 redirect 到 7892，UDP 全部 TProxy 到 7893 并打标记 `0x233`；策略路由 `fwmark 0x233 lookup 233`，表 233 是 `local 0.0.0.0/0 dev lo`。双网卡网关另加一张 `ip atom_nat` 表（LAN → WAN 的 masquerade，现在只剩非 TCP、UDP 的包会用到，比如 ping）和 `ip_forward = 1`；单网卡旁路由不需要 `ip_forward`。原来按两个集合 `fakeip4`、`iprule4` 挑选目标，集合由 atomdns 维护（`singbox.nft_table`、`NftSets`、`sync_nft`、`status.nft`）；改成全部导入后这些都删了，规则文件每次加载整表替换，不再需要 atomdns 启动后填集合。
- **sing-box 自己的流量**：nft 只挂 `prerouting`，本机进程的包走 `output`，不会被导回来，所以不设 `routing_mark`、也没有排除标记。本机进程访问 FakeIP 不处理。
- **UDP 53 的写回**（联调发现，用户选的修法）：sing-box 的 TProxy 回包要用 IP_TRANSPARENT 绑定"原目标地址:端口"，atomdns 在 `*:53` 监听且没设 `SO_REUSEADDR` 时，客户端发往 FakeIP 或 IP 规则地址的 UDP 53（比如 IP 规则里有 8.8.8.8，客户端直接问它）绑定报 `EADDRINUSE`，回包发不出去。改为 atomdns 在通配地址（`[::]`、`0.0.0.0`）上的 UDP 监听设 `SO_REUSEADDR`（`rust/native-host/src/bind.rs`）。代价：同一台机器上再起一个监听通配地址的 atomdns，不会再报端口被占用，而是共用 53 端口；具体地址的监听照旧拒绝重复绑定。
- **拓扑**：单网卡旁路由和双网卡网关都在 netns 里实测过（deploy-nft.md 第 7 节）。单网卡时主路由只看到去程，转发链里"丢弃 invalid"的规则可能丢包，这一点没有实测条件，文档给了三种处理办法，其中之一是安装器的"回包经主路由"选项（2026-11-14，deploy-nft.md 3.4）。

### 9.5 arm64 和 Alpine（第 11 步）**[定]**

2026-11-14 用户定：在 Debian/Ubuntu + x86_64 之外，支持 arm64 和 Alpine（OpenRC），四种组合都用同一套安装器。armv7 不做；Docker/LXC 里的 Alpine 不支持，环境检查拦下；Alpine 最低 3.19。

- **包**：四个，`atom-<版本>-linux-{amd64,arm64,amd64-musl,arm64-musl}.tar.gz`。atomdns 按 libc 选：Debian/Ubuntu 用 glibc 版（`amd64`、`arm64-gnu`，不需要比 2.36 新的 glibc），Alpine 用 musl 版（`amd64-musl`、`arm64`），都取自 atomdns `release-build.yml` 的产物。不全换 musl：atomdns 没有换内存分配器，musl 的 malloc 在多线程下慢。sing-box 和 `atom-installer` 是 `CGO_ENABLED=0` 的静态程序，每个架构编一次，glibc 和 musl 包共用。`build-package.sh` 一次出四个包、一个 `install.sh`（写进四个 sha256）和 `SHA256SUMS`。
- **引导脚本**：按 `uname -m`（`x86_64`、`aarch64`）和 `/etc/os-release` 的 `ID`（`debian`、`ubuntu`、`alpine`）选包；要 systemd 或 OpenRC 之一。Alpine 的命令是 `apk add bash curl && curl -fsSL <地址> | bash`（脚本仍是 bash）。向导在 systemd 上用 `systemd-run`，在 OpenRC 上用 `start-stop-daemon --background --make-pidfile`；关闭向导时等的是 pidfile 里的进程退出。
- **服务**：安装器里一层很薄的服务接口，systemd 和 OpenRC 两个实现（写服务定义、生效、enable/disable、start/stop/restart、is-active、最近日志）。OpenRC 的 `/etc/init.d/atom-dns`、`/etc/init.d/atom-sing-box` 用 `supervise-daemon` 自动重启；nft、sysctl、`policy-route.sh` 放在 `start_pre()`；sing-box 另有 `reload`（发 HUP）；日志在 `/var/log/atom/`。`atom_rules.yaml` 里 sing-box 的 `reload` 在 OpenRC 上由安装器每次安装、升级时改写为 `/opt/atom/lib/reload-sing-box.sh`（在运行就 reload，没运行就 start，同 `reload-or-restart`），atomdns 子模块不改。
- **依赖**：Alpine 用 `apk add nftables iproute2`，始终要 iproute2（busybox 的 `ip` 不保证策略路由，也没有 `ss`）；`sysctl` 用 busybox 的。装过的包记进 `changes.json`，卸载时和 Debian 一样不卸载软件包。
- **53 端口（Alpine）**：占用进程沿父进程链对上 `/run/openrc/daemons/<服务>/001` 里的 pidfile，就是那个 OpenRC 服务；"停用"按钮 `rc-service <服务> stop` 并从所在运行级别删掉（`rc-update del`），原来的运行级别记进 `changes.json`，卸载时加回并启动。对不上服务的进程只提示。`/etc/resolv.conf` 只有回环地址时，依赖先装，停用后把它改成默认网关，原内容备份，卸载时恢复。
- **验证**：Alpine amd64 用测试机 `ssh alpine`（Alpine 3.23，linux-virt 6.18，内核有 `nft_tproxy`，2026-11-14 查过；上面原来跑着 atomdns 和 match-router，atomdns 用来测"停用"按钮；match-router 是这台机器出网的路径，测试时没有停）；arm64 用 `ssh n1`（斐讯 N1，Armbian 25.08 bookworm，`ID=debian`、`VERSION_ID=12`，glibc 2.36，systemd，ophub 6.1 内核，`nft_tproxy`、`nft_socket` 是模块；没装 nftables，systemd-resolved 的 stub 占着 53，NetworkManager 管网卡；2026-11-14 只读查过）。

- **实施和验证记录（2026-11-14）**：
  - **11a 打包**：`build-package.sh --atomdns-dir DIR` 一次出四个包、`install.sh`、`SHA256SUMS`；构建前检查每个 atomdns 的 ELF 架构、glibc 包不要求比 2.36 新、musl 包不含 glibc 符号；sing-box 版本取自 `git describe`。用假 atomdns 加真编的 sing-box 和安装器跑过，选包逻辑用伪造的 `uname -m` 和 os-release 测过各组合。
  - **11b arm64（`ssh n1`，Armbian，Debian 12，systemd）**：安装器的系统检查放行 arm64。本机（Apple Silicon）用 cargo-zigbuild 编 `aarch64-unknown-linux-gnu.2.35` 的 atomdns（最高要 GLIBC_2.34），打成 arm64 包，只上传这一个包；`ATOM_PKG` 起向导，浏览器里走完单网卡旁路由，点"停用系统 DNS 监听"，向导用 apt 装 nftables（装完的 man-db 触发器很慢，不是卡住）。装完两个服务 active、`inet atom` 表和 `ip rule fwmark 0x233` 在、`ip_forward` 保持 1、面板 200、`www.google.com` 得到 FakeIP、`www.baidu.com` 得到真实 IP；API 改 mixed 端口后 sing-box 重载并监听新端口。0.1.0 → 0.1.1 升级成功（向导被 `stop_wizard` 关掉）。`uninstall --purge --yes` 后 resolved 的 stub 和 `/etc/resolv.conf` 链接还原，nft 表、`ip rule` 清掉，`ip_forward` 回到 1；之后手动 `apt remove` 了 nftables，机器回到干净状态。
  - **11c Alpine（`ssh alpine`，Alpine 3.23.5，OpenRC）**：本机用 cargo-zigbuild 编 `x86_64-unknown-linux-musl` 的 atomdns，打 amd64-musl 包。`ATOM_PKG` 加 `start-stop-daemon` 起向导，pidfile 正确；向导认出占 53 的是 OpenRC 服务 `atomdns`，点"停止并禁用 atomdns"：先装 nftables，resolv.conf 从 `127.0.0.1` 改到默认网关并备份，`atomdns` 从 `default` 运行级别删掉。单网卡旁路由安装，面板端口 8080：两个服务在 `default` 运行级别、`inet atom` 表和 `ip rule 233` 在、面板 200、google 得到 FakeIP、baidu 得到真实 IP；API 改 mixed 端口后 sing-box 重载；`kill -9` sing-box 两次，supervise-daemon 都拉起；整机 reboot 后服务、nft、策略路由都自己起来；0.1.0 → 0.1.1 升级成功；`uninstall --purge --yes` 后 `atomdns` 回到 `default` 并在运行，nft 表、`ip rule`、`/opt/atom`、init 脚本都清掉，resolv.conf 还原。`/var/log/atom` 当时没删，后来改为卸载时删，只有单元测试覆盖，没在机器上再卸载一次（残留目录手动删了）。测完机器的运行级别、路由、`ip_forward`、软件包列表、apk 源与测前一致。
  - **测试时的情况**：alpine 靠 match-router 出网，所以没有停它，面板用 8080 避开它的 80；这台机器连不上 `dl-cdn`，测试时临时把 apk 源换成阿里云镜像，测完还原；`apk update` 失败时"停用"会报错并中止，atomdns 保持原样。
  - **没验证**：只有 arm64（glibc）和 amd64-musl 两个包用真实程序测过，amd64（glibc）和 arm64-musl 只用假 atomdns 测了打包和选包；双网卡网关在两台机器上都没测；没有从 GitHub 实际下载；Docker/LXC 的拦截只有单元测试，没在容器里跑；Ubuntu、Alpine 3.19 到 3.22 没测。
  - **已知限制**：用"停用"按钮停一个 OpenRC 服务时，OpenRC 会连带停掉 `need` 它的服务，这些服务没有记录，卸载时不会还原；常见的 DNS 服务只被别的服务 `use`，所以少见。

## 10. 仓库与版本

```text
atom/          jasonxtt/atom
  README.md  LICENSE
  docs/        设计、补丁说明、部署文档
  atomdns/     子模块 → jasonxtt/atom-core，分支 atom/main（自带的 YAML 配置在 atomdns/config/）
  sing-box/    子模块 → jasonxtt/sing-box，分支 atom/fakeip-outbound
  installer/   安装器（Go 程序 + 一条命令的脚本 + 打包脚本）
```

- fork 自带的 YAML 配置放在 `atomdns/config/`：从 `jasonxtt/file` 的 `atomdns/config/full/config_rust`（v0.6.4，schema 7）复制过来，锁定改动直接改这份，不动 file 仓库，改动记在 `atomdns/config/README.md`。**[定]**
- **开源（2026-11-13）[定]**：三个仓库都公开，都是 GPL-3.0；两个 fork 保留原有的 LICENSE，是衍生作品。
  - AtomDNS fork 放在新仓库 `jasonxtt/atom-core`（GitHub 不能 fork 自己的仓库），内容是 `jasonxtt/atomdns` 的 `atom/main` 分支和它的完整历史；程序名、服务名不变（`atomdns`、`atom-dns.service`）。本地 `atomdns/` 的 `origin` 仍指 `jasonxtt/atomdns`，用来跟进上游，remote `atom` 指 atom-core。上游的版本标签不推到 atom-core。
  - sing-box fork 是 GitHub 上 `SagerNet/sing-box` 的 fork `jasonxtt/sing-box`，分支 `atom/fakeip-outbound`。本地 `sing-box/` 的 `origin` 是上游，remote `atom` 是 fork。
  - atom 用子模块固定两个 fork 的提交，子模块的提交就是"配套版本"。`git clone --recursive` 拿到全部源码。
- **版本 [定]**：配套包的版本就是 atom 的版本，2026-11-13 开源时从 0.1.0 重新开始（之前的 v0.1.0～v0.1.2 测试 Release 和标签删掉，测试机上用过的 0.1.3～0.1.16 只在本地打过包）。Release 的标签指向包含子模块的 atom 提交；包里的 `manifest.json` 记录 atomdns、sing-box、安装器三处的提交。安装器只认配套的组合，不单独升级某个组件。

## 11. 实施顺序 **[建议]**

1. **AtomDNS fork 打底**（已完成）：从 main 建分支；锁定 7.1 里的项，删除 RealIP、专属分流组和在线更新；加上 EDNS0 65001 编码，Final 路径发 CLEAR。验收：沿用现有名单，链路能跑通，FakeIP 带上 CLEAR。
2. **规则表**（已完成，实现见 4.7）：模型、首条命中匹配插件、`rules` API、拖动、`rules/test`；规则命中"代理"时发 SET。另外做了 `ruleset` 条件的全部三种来源（内置、远程 URL、本地列表）和 `rulesets` API。**[定]** 还没做：远程规则集的定时自动更新（现在只能手动 `refresh`）。
3. **sing-box 配置生成**（已完成，2026-11-09 收尾，经过三轮 codex 复审；实现见 4.6、5.3、第 6 节）：节点导入（JSON/YAML/链接）、节点组、Final、事务式应用和回滚；7890 按规则分流，包括 sing-box 补丁里 resolve 之后查绑定的那一处。还没做：nft 规则（第 4、7 步）、systemd 单元文件（第 7 步）。
4. **联调**（已完成，2026-11-09；实现和定下的事见 4.6、9.4，规则见 [deploy-nft.md](deploy-nft.md)）：在 mosdns-rust 上用临时 systemd 单元跑 atomdns 和补丁版 sing-box，客户端用 netns 模拟，单网卡旁路由和双网卡网关两种形态都实测。验证通过的：DNS（代理规则 → FakeIP 且 sing-box 收到 SET，AAAA 空应答也带 SET；直连 → 真实 IP；屏蔽 → 空应答；没命中的国外域名 → FakeIP + CLEAR；国内域名 → 真实 IP；EDNS 选项用抓包核对）；TCP 经 redirect、UDP 经 TProxy（NTP、DNS）进 sing-box，从绑定的组或 Final 出去，UDP 回包源地址是 FakeIP；IP 规则（TCP、UDP、屏蔽，启用/停用时 nft 集合跟着变）；7890 按规则分流；拖动排序和改组立即生效，删掉代理规则后下一次查询重新发 CLEAR（缓存已清）；sing-box 重启后不重新查 DNS 绑定仍在；atomdns 重启后规则、节点、组、Final 不变，启动检查不重载 sing-box，nft 表重新加载并填好；`sing-box check` 不通过返回 400 什么都不动；mixed 端口被占时重载失败返回 502 并回滚，sing-box 进程不退出（PID 不变），期间 7890 请求 40/40 成功；网关形态下其他流量经 NAT 转发、不进 sing-box。联调中修的：UDP 53 写回（9.4）。codex 复审三轮：第 1、2 轮的两条 MAJOR（应用失败回滚、保存失败补偿后 nft 没跟上运行中的配置）已修；第 3 轮的一条（补偿时重载也失败）按已知限制处理，见 9.4 和 deploy-nft.md 第 8 节。没做：真实主路由下的单网卡形态、真实节点。
5. **绑定账本和缓存失效**（已完成，2026-11-09；实现见 4.5.1）：绑定账本（SET 和 CLEAR 都记，LRU 16384 条，规则或规则集改动、确认删组后同步重算，有变化的直接发给 sing-box 的 `dns-in`，改 FakeIP 网段时清空）；直连应答 TTL 上限 `direct_ttl_max`（默认 300 秒）和 FakeIP 应答 TTL 上限 `fakeip_ttl_max`（默认 1 秒）。验证（mosdns-rust，第 4 步的临时环境，atomdns 换成第 5 步的调试版，通过运行时的 systemd drop-in 指向 `/root/atom-step5/bin`，没有永久替换）：客户端拿着 FakeIP 不重新查询，规则从组 X 改成组 Y 后连接从 Y 出去，抓包看到账本发出 SET Y；删掉代理规则后 sing-box 收到 CLEAR，同一个 FakeIP 走 Final；代理改直连、改屏蔽分别发 SET direct、SET block，同一个 FakeIP 立即直连、被拒；原判断走 Final 的国外域名加了代理规则后，同一个 FakeIP 改走新组；确认删组后发 SET final；sing-box 重启后绑定仍在；改 FakeIP 网段后账本清空；FakeIP 应答 TTL 是 1，改 `fakeip_ttl_max` 后跟着变；直连应答（上游 TTL 86400）封顶到 300，改 `direct_ttl_max` 后跟着变，UDP、TCP 和缓存命中都一样；改 TTL 设置不重载 sing-box。全量 `cargo test --workspace` 1637 个通过、0 失败（最终提交）。codex 复审三轮：第 1 轮两条 MAJOR（AAAA 应答被当作确认，已改成只记 A；重算不模拟重写、DDNS 等固定项，记为限制），第 2 轮一条 MAJOR（sing-box 绑定保存失败仍照常应答，sing-box 补丁改成报错、账本只认 NOERROR），第 3 轮通过。sing-box 补丁多了一个提交（`e7b78640`），端到端用的是它的 Linux 构建。
6. **简易 WebUI + OpenAPI 规范定稿**，拆成两步 **[定]**：
   - **6a 后端定稿**（已完成，2026-11-09；见 4.4、7.1、7.2、第 8 节）：OpenAPI 规范 `atomdns/docs/api/openapi.yaml`（redocly lint 无错误无警告，`ROUTES` 一致性测试）；新接口 `dns`、`upstreams`、`ddns`、`rewrite`，内置规则集可以刷新；删除专属分流组的代码、AtomDNS 原有的管理接口（27 条在测试机上逐条确认是 404）和背后只给它们用的写入路径；黑名单、白名单、灰名单删除，`cuscn`、`cusnocn` 成为内置规则集并进入默认规则表。全量 `cargo test --workspace` 1531 个通过、0 失败（删掉了只测已删接口的测试，借旧接口做探针的测试改走进程内入口）。测试机（mosdns-rust，运行时 drop-in 指向 `/root/atom-step6/bin/atomdns` 和新配置根 `/root/atom-step6/root`，sing-box 仍是第 5 步的构建，没有永久替换）上新装状态验证：默认规则表 4 条；代理修正的域名拿到 FakeIP、直连修正的拿到真实 IP；`dns`、`upstreams`、`ddns`、`rewrite` 改了即生效、重写的域名直接应答；`cusnocn`、`geosite_cn` 经 7890 刷新成功。客户端分流（`client_routing`）删除，规则的客户端条件加了取反（`client_invert`，4.2）。没做：`HOST_API_LEVEL` 升级放到 6b，和新 WebUI 一起。
   - **6b 简易 WebUI**（已完成，2026-11-11）：新写的 Vue 3 + Vite + TypeScript 工程（`atomdns/webui/`），按规范实现，删除 `webui-log/`，测试机上跑完整流程。
     - 分段：6b-1 后端小接口（系统资源、`groups/move`、审计接口进规范、节点最近延迟、连接转发）；6b-2 规则模型扩展（规则 DNS 上游组和缓存开关、自定义上游组、DDNS 并入规则、规则源目录和域名/IP/全部、内置规则集改地址和删除、每日自动更新）；6b-3 前端（3a 骨架/概览/设置，3b 节点组和节点，3c 规则和规则集，3d DNS 和连接）。每段 codex 审查，三轮不过的按用户决定修。
     - 接口类型由 `npm run gen:api` 从 `atomdns/docs/api/openapi.yaml` 生成，构建时做类型检查。界面包 `atom-webui.tar.gz`（`npm run build:package`），装到 `<配置根>/ui/main/`。`HOST_API_LEVEL`、`MIN_UI_API_LEVEL` 都是 2，旧的 1 级界面包被拒绝挂载。
     - 和 demo 的差别：规则只选上游组（不单选上游）；DNS 概览只展示审计接口给的数据（记录的查询、平均耗时、时间窗、热门域名/客户端/命中分类），没有缓存命中率、屏蔽数和每个上游的耗时/错误率（没有接口）；审计的开关、容量、清空没做界面（这几项 2026-11-12 补上，见第 8 条）；正式界面不放 demo 的说明性文字。连接表的结构参照 zashboard（MIT，`webui/licenses/zashboard-MIT.txt`）。
     - 验收（测试机 mosdns-rust，临时单元，打包版）：16 个页面都能读到数据、没有报错；在界面上新建一条屏蔽规则，`dig` 得到空应答，就地改成代理后同一域名马上拿到 FakeIP，删除后恢复。各页面另做过逐项实测（导入节点、节点组拖动和切换、规则源目录自动下载并把 IP 交给 sing-box、上游组新建和保存、设置保存、连接关闭等）。
     - 所有编辑页查过两类问题：读取没成功也能编辑或保存（而保存会整份替换服务器上的数据），共享数据被晚到的旧回复覆盖。前者在规则编辑、节点组编辑、本地列表、重写里都改成读取成功才能编辑、失败给重试；后者在共享状态里按版本号（节点/节点组/兜底、规则表）或请求顺序（规则集、上游组、运行时、状态）丢弃旧回复。
     - **已知限制**（6b-3b，codex 第 3 轮，用户决定不改）：
       - 编辑节点时遇到 409，冲突面板只列名称、类型、地址和端口。别人只改了传输参数时，用户看不出来，点"仍然保存"会覆盖掉。家用单人管理，概率低。
       - 整组测速只把直接成员里没回结果的节点记为失败。sing-box 会递归测子组，子组里失败的节点不记失败，可能显示"未测速"。
       - 测速失败只记在页面内存里（sing-box 测速失败会清掉该节点的历史），刷新浏览器后丢失。
7. **安装器**（已完成，2026-11-12；设计、行为和验证见 9.2，说明见 [installer/README.md](../installer/README.md)）：一条命令加网页向导，装到 `/opt/atom/`，支持升级、卸载、保留数据重装、失败撤销；配套包由 `installer/scripts/build-package.sh` 组装。同时清理了 atomdns 里旧的发布流程（在线更新清单、Docker 镜像脚本和工作流、`docs/rust/docker.md`，`release.md` 改写成 atom 的打包步骤）。

8. **DNS 页补全**（2026-11-12）：用户反馈新 DNS 页比原版 AtomDNS 少了很多，fork 里仍成立的都补上。
   - 菜单：概览、日志、上游、高级设置。概览照原版布局：查询数量、平均响应、缓存命中（各缓存命中数之和 / 查询数之和）、域名统计（不同域名数）；上游统计表（每个上游的请求数、采纳率、出错率、平均延迟，按组分段，含自定义组和 FakeIP 组）；分流状态环形图（规则命中占比，前 5 加"其他"，点图例看明细）；热门域名、活跃客户端、最慢查询。排行和图例都能点开明细（分页），明细里的日志行能点开单条详情。日志页点行看详情（基本、应答、分流与上游、上游尝试）。高级设置（宽屏时第一行是解析、重定向 / hosts、TTL 上限和查询日志，第二行缓存和域名学习并排，变窄后分行）：解析（核心模式、ECS、IPv4 优先、IPv6 屏蔽、类型屏蔽、空结果屏蔽）、缓存（两个开关叫"全局缓存""上游应答缓存"，每个缓存一行：条目、命中率、容量、过期保留、写盘间隔、清空）、域名学习（总开关、四类、定期刷新）、TTL 上限、查询日志（记录、容量、清空）、重定向 / hosts（原"重写"）。缓存的容量、过期保留、写盘间隔只显示不修改。
   - 后端：第 8 节表格里的 `caches`、`stats/upstreams`、`learning`、`requery`；`dns` 开放 `block_empty`；审计日志输出 `cache_status`；`lazy_cache` 管 `cache_atom_<组>`（4.8）；规则命中的域名不再学"无 V6"（4.8）；清缓存收窄（4.5.1 的"清缓存"，含"发布新一代配置时全清"的限制）。
   - 不恢复：DNS 分流模式、国内域名 FakeIP、黑白灰名单、广告总开关、客户端分流、DDNS、专属分流组、SOCKS5、学习状态面板、诊断抓取。

9. **规则页：原判断和内置规则集选择**（2026-11-12，用户定）**[定]**：
   - 规则列表最后固定一行"未命中以上规则"，带 DNS 标签：这部分是 AtomDNS 的 DNS 判断（3.1 第 3 层），不是 sing-box 的路由规则。不能拖动、编辑、删除。展开后按配置里的实际顺序列出 A/AAAA 查询的分支（`sequence_6666` 规则表之后、`process_listed.yaml`、`process_v4/v6.yaml`）：学到的无 V4 / 无 V6 → 空应答；`geosite_no_cn` → FakeIP · 代理 · 兜底分组；`geosite_cn` → 直连 · 国内 DNS；记忆代理 → FakeIP · 代理 · 兜底分组；记忆直连 → 直连（兼容模式国内 DNS，安全模式国外 DNS）；其余 → 按核心模式探测（兼容国内 DNS，安全国外 DNS）：国内 IP 或 `direct_ip` 直连，其他 IP FakeIP · 代理 · 兜底分组，上游没有给 IP 的不给地址（AAAA 学"无 V6"；兼容模式下 AAAA 失败也走 FakeIP，sing-box 对 AAAA 不给地址），并学习。注意 `geosite_no_cn` 排在 `geosite_cn` 前面、记忆代理排在记忆直连前面，这是 YAML 的顺序。
   - 开关按 `GET dns` 的实际状态显示：空应答那一支要"空结果屏蔽"开着、对应学习类别生效；学习类别生效 = 总开关且类别开关（和 `route_learner` 编译时一样）；关掉的分支标"关"。跳转：geosite → 规则集页对应的默认规则集（`/rules/sets?set=<id>`），学习类分支 → DNS 高级设置的域名学习，其余 → 核心模式（`/dns/settings?at=learning|mode`）。代码 `webui/src/original.ts`（测试 `tests/original.test.mjs`）。
   - 新建条件的规则集选择框只列 MetaCubeX 和 blackmatrix7 的目录条目，不再列默认规则集（`adguard`、`geosite_cn`、`geosite_no_cn`、`cuscn`、`cusnocn`）。已经引用默认规则集的规则（默认规则表的两条修正，以及旧安装里的广告规则）照常显示、编辑、保存，可以删掉这个条件；删掉以后不能再从选择框选回来（用户接受：广告规则改用目录里的规则集）。

10. **规则页：测试域名改成 dry-run**（2026-11-12，用户定）**[定]**：测试框调用 `POST rules/dryrun`（第 8 节），按真实流程的顺序列出每一步命中与否和原因，给出结论；代理时把组按 sing-box 当前选中项展开到节点；对照绑定账本。探测真的查上游但不留痕迹，不读不写缓存。代码 `webui/src/dryrun.ts`（测试 `tests/dryrun.test.mjs`）、`RulesPage.vue`。

11. **arm64 和 Alpine**（2026-11-14 定，见 9.5）：11a 打包（四个包，`install.sh` 按架构和系统选包）；11b arm64（systemd），在 Armbian 上验证；11c Alpine（OpenRC 服务、apk、53 端口、改写重载命令），在 `ssh alpine` 上验证；11d 更新 9.2 和 `installer/README.md`。**已完成（2026-11-14）**：11a～11d 都做完，实测记录见 9.5。

每一步都单独验收后再进行下一步。实施顺序已认可。**[定]**

## 12. 审阅结论（2026-11-08）

- 第 12 节原来的 6 个问题都已答复，答复已写回上面对应的章节。
- 没有单独提到的 **[建议]** 项，用户都认可，按建议执行。
- 国外 DNS 走 SOCKS（mixed 7890），见 7.1；7890 的流量也按规则分流，见第 6 节，在第 3 步实现。**[定]**
- 目前没有等待确认的项。
