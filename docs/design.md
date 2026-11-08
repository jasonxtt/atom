# atom 设计文档 v0

状态：v0 已审，第 12 节的问题已答复；第 2 步实现时新定的事项写回了第 3、4、8 节。日期：2026-11-08。

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
2. **绑定账本**：AtomDNS 在内存里记录"给 sing-box 发过 SET 的域名 → 组"，用 LRU 限制大小。规则变化后，用新规则重算这些域名，把有变化的 SET/CLEAR 直接用 DNS 查询发给 sing-box，不需要新接口。这样"组 X 改成组 Y"可以立即生效。**[建议]**
3. 直连和代理互换的情况，客户端手里拿的是真实 IP，只能等它的 DNS 缓存过期。直连应答的 TTL 要设上限（可配置，默认 **[待定]**）。

### 4.6 IP 规则

目标是 IP 的规则不经过 DNS，AtomDNS 处理不了。控制面把它们编译成 sing-box 的 `ip_cidr` 路由规则，同时让 nft 把这些网段导进 sing-box。这是唯一一类在 sing-box 里匹配的规则。**[建议]**

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
- **持久化**：规则表存在 `webinfo/atom_rules.json`（`{version, rules}`），规则集存在 `webinfo/atom_rulesets.json`，列表内容存在 `webinfo/atom_rulesets/<id>.txt`，都是写临时文件再改名。每次修改先校验、编译出新的匹配表，再写盘，最后整张表替换（`Live`）：查询要么看到旧表，要么看到新表，DNS 不中断。文件读不了或解析不了时，配置编译失败，程序不启动，不会悄悄换成默认表。**[定]**
- **group_id**：组到第 3 步才有，现在只校验格式：非空、没有首尾空白和控制字符、最长 64 字节。`atom_rules.rs` 的 `validate_group` 是以后检查"组存在"的位置。
- **ip_cidr**：API 先拒绝，第 3 步和 sing-box 配置生成一起做（4.6）。

## 5. 节点与节点组

### 5.1 节点 **[定]**

- 输入格式：sing-box outbound JSON、YAML、分享链接（`vless://`、`vmess://`、`trojan://`、`ss://`、`hysteria2://`、`tuic://` 等）。
- 统一转换成 sing-box outbound 后保存。保存前用 `sing-box check` 校验整份生成的配置。**[建议]**
- 分享链接的解析在 atom 的 Rust 代码里实现。**[建议]**

### 5.2 节点组

已定：

- 类型先做 `selector` 和 `urltest`，接 mihomo 时再加 fallback 等。**[定]**
- **"默认代理"是内置组，不能删除**，至少要有一个节点。**[定]**
- **Final** 是兜底，由原判断的"国外"结果和所有没绑定的 FakeIP 流量使用。它只能引用一个节点组，默认是"默认代理"，**不能设成 direct**，由 API 校验强制。**[定]**
- 节点和组在 sing-box 里的标签用不可变的内部 ID（如 `n-1a2b`、`g-7f3a`），名称只用于显示。这样改名不会让已有绑定失效。代价是第三方 Clash 面板里看到的是 ID。**[定]**

- **Final 做成 selector**，成员是节点组，默认选中"默认代理"。运行时通过 Clash API 切换，不用重载。`route.final` 指向这个 selector。**[定]**
- **组可以嵌套引用其他组。** 保存时做环检测，有环就拒绝。**[定]**
- **urltest 参数**：只开放测速 URL、间隔、容差三项，都有默认值，用户可以改。**[定]** 默认值沿用 sing-box 的：`https://www.gstatic.com/generate_204`、3m、50ms。**[建议]**
- **删除被规则引用的组**：API 先返回 409 和引用它的规则列表；UI 弹出确认，用户确认后带 `confirm=true` 再删一次，那些规则的动作自动改成 Final。嵌套引用了它的组，同样从成员里移除。**[定]**
- **切换后端（sing-box ↔ mihomo）**：selector 保持不变；sing-box 不支持的类型（fallback、load-balance 等）一律提示用户，并自动转换成 urltest。**[定]**

## 6. sing-box 配置生成 **[建议]**

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

`core_mode`（兼容/安全模式）、类型屏蔽、IPv4 优先、缓存开关和容量、国内外上游 DNS 列表、ECS、域名学习、FakeIP 网段（改了要连同 nft 规则一起重新生成）、直连应答的 TTL 上限。

### 7.3 跨组件的一致性

由控制面一处修改、同时下发到各组件：

- FakeIP 网段：sing-box 的 `inet4_range`、nft 规则、主路由静态路由（文档提示用户配置）。
- IPv6：v0 不做 IPv6 FakeIP；被代理域名的 AAAA 查询返回空应答。**[建议]**
- IP 规则网段：sing-box 路由规则和 nft 规则。

## 8. 管理 API（骨架）**[建议]**

正式规范用 OpenAPI 3 写在 `docs/api/openapi.yaml`，WebUI 和第三方 UI 都按它来实现。路径前缀暂定 `/api/v1/atom/`，和 AtomDNS 现有的 API 分开。

| 资源 | 操作 |
|---|---|
| `rules` | 列表、新建、修改、删除、`POST rules/move`（拖动）、`POST rules/test`（输入域名，看会命中哪条规则） |
| `rulesets` | 列表、新建（内置 geosite / 远程 URL / 本地列表）、刷新远程、删除 |
| `nodes` | 列表、导入（`format: json|yaml|link`，可以批量）、修改、删除 |
| `groups` | 列表、新建、修改、删除（"默认代理"不可删）、`POST groups/{id}/select`、`POST groups/{id}/delay` |
| `final` | 读取、设置（只能指向一个组） |
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
  - 错误体都是 AtomDNS 现有的 `{"error": "..."}`。写操作和 `rulesets` 的下载都占用管理租约，和设置、配置重载互斥。
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
3. **sing-box 配置生成**：节点导入（JSON/YAML/链接）、节点组、Final、事务式应用和回滚；7890 按规则分流，包括 sing-box 补丁里 resolve 之后查绑定的那一处。
4. **联调**：在测试机上组装 VM 环境，配好 nft，做端到端验证（TCP/UDP、拖动后立即生效、重启后恢复）。
5. **绑定账本和缓存失效**（4.5）。
6. **简易 WebUI + OpenAPI 规范定稿**。
7. **安装器**。

每一步都单独验收后再进行下一步。实施顺序已认可。**[定]**

## 12. 审阅结论（2026-11-08）

- 第 12 节原来的 6 个问题都已答复，答复已写回上面对应的章节。
- 没有单独提到的 **[建议]** 项，用户都认可，按建议执行。
- 国外 DNS 走 SOCKS（mixed 7890），见 7.1；7890 的流量也按规则分流，见第 6 节，在第 3 步实现。**[定]**
- 目前没有等待确认的项。
