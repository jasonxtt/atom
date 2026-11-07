# atom

一台 VM 同时跑 DNS 和代理的分流网关。由 DNS 统一决定每个域名走哪个出口，代理核心只负责执行，不再做第二次判断。

```text
atom（AtomDNS fork：DNS + 规则表 + 控制面）── DNS + EDNS0 65001 ──> sing-box（fork）
        └──────────── 生成配置 / SIGHUP / Clash API ──────────────────┘
```

- 设计文档：[docs/design.md](docs/design.md)（v0 草稿，待审）
- sing-box 补丁：[docs/singbox-fakeip-outbound.md](docs/singbox-fakeip-outbound.md)

## 目录

| 路径 | 内容 |
|---|---|
| `docs/` | 设计、补丁说明，以后加 API 规范和部署文档 |
| `sing-box/` | sing-box fork，分支 `atom/fakeip-outbound`，基于 v1.14.4 |
| `atomdns/` | AtomDNS fork（还没建） |

## 进度

- [x] sing-box FakeIP 出口绑定补丁
- [ ] 设计文档 v0 审阅
- [ ] AtomDNS fork 打底：锁定选项、EDNS0 65001 编码
- [ ] 规则表与拖动排序
- [ ] sing-box 配置生成：节点、节点组、Final
- [ ] 测试机联调
- [ ] 绑定账本与缓存失效
- [ ] 简易 WebUI 与 OpenAPI 规范
- [ ] 安装器
