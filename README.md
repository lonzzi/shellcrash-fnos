# ShellCrash for 飞牛 fnOS

将 ShellCrash 兼容配置、官方 Mihomo Core 和 MetaCubeXD 面板打包为 fnOS 原生 FPK。Core 和订阅管理器直接运行在 fnOS 宿主机上；不需要 Docker，也不把 TUN 限制在容器网络里。桌面图标通过 fnOS 网关在内嵌小窗口打开订阅管理页，高级面板也留在同一个窗口。

这是社区维护的第三方封装，不隶属于 ShellCrash、MetaCubeX 或飞牛官方。上游配置与许可证信息随包提供。

## 安装和升级

1. 从 [Releases](https://github.com/lonzzi/shellcrash-fnos/releases) 下载与你的 fnOS 设备架构相符的 FPK：x86_64 选 `x86`，ARM64 选 `arm`。
2. 在 fnOS 应用中心手动安装。应用会创建初始配置和本机 API 密钥；只有管理员可以从桌面入口打开控制界面。
3. 在订阅管理页填入完整 Clash/Mihomo YAML 订阅链接，设置 1 到 720 小时的更新间隔。管理器保留订阅中的 `proxy-providers`、策略组、规则和节点，不会擅自把 provider 填进组。
4. DNS 和 TUN 可以分别选择跟随订阅、开启或关闭。保存会检查新配置；失败时会恢复之前的 profile、runtime 和 ShellCrash 设置。

从旧 Docker 版升级时，升级脚本会先停掉旧容器，并在 `/vol1/@appdata/shellcrash-fnos/.codex-backups/` 创建权限为 `600` 的完整配置与密钥归档。新 Core 启动且健康检查通过后才会删除旧容器。已有订阅、providers、策略组、API 密钥、DNS/TUN 覆盖和应用数据会保留；若原生服务未就绪，启动钩子会尝试重新启动旧容器。卸载默认保留配置数据。

## 整机 TUN 和代理端口

启用 TUN 后，Mihomo 作为 fnOS 宿主机 root 服务运行，使用 Linux TUN、`auto-route` 和 `auto-redirect`。这会在宿主机网络空间设置路由；自动重定向用于 TCP，UDP 等流量由 TUN 路由接管。环回、链路本地和常用私网地址段会加入排除列表，以免影响本机和局域网管理连接。实现参考 [fnOS Mihomo 原生 FPK 示例](https://github.com/conversun/fnos-apps/tree/main/apps/mihomo)，TUN 字段遵循 [Mihomo TUN 文档](https://wiki.metacubex.one/en/config/inbound/tun/)。

订阅的规则与策略决定 TUN 捕获后的流量走代理节点还是 `DIRECT`。所以“Core 已连接”和“TUN 接口运行”代表宿主机路由已启用；如果希望公网请求经代理出去，还要在 Mihomo 面板中确认当前策略组选中了可用节点，并让规则命中该组。订阅若默认 `MATCH,DIRECT`，整机流量会经 Mihomo 处理但不会经代理服务器转发。

- HTTP/SOCKS 混合代理监听 NAS 的 TCP/UDP `17890`，可供局域网设备显式使用。
- Mihomo 控制器只绑定 `127.0.0.1:9999`，管理器只绑定 `127.0.0.1:9998`，不额外暴露管理端口。
- `17890` 流量端口按 fnOS 应用端口配置开放；不要把它直接暴露给不可信网络。
- 桌面小窗使用 fnOS 管理员登录态；API 密钥保存在应用配置目录，权限为 `600`，不会下发到浏览器。高级 MetaCubeXD 面板通过 fnOS 网关自动连接，不再要求手动输入密钥。前端只使用无权限的连接占位值，网关会替换 REST 和 WebSocket 认证；未登录或非管理员仍无法访问。
- 高级 MetaCubeXD 面板随 FPK 离线安装。右下角的“返回 ShellCrash 订阅管理”可以回到管理页，关闭 fnOS 窗口可返回桌面。

状态页会显示 Core API 是否连接、TUN 接口是否处于 UP 状态和混合代理端口。可从 NAS 终端用不带代理环境变量的请求做整机 TUN 测试，例如 `env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY curl -I --max-time 20 https://www.google.com`。再从高级面板的 Connections 检查这条请求实际命中的规则、代理链和流量。空闲时实时流量为 0 是正常现象。

ShellCrash 配置保存在 `/vol1/@appdata/shellcrash-fnos/ShellCrash/`，本机 API 密钥保存在 `/vol1/@appconf/shellcrash-fnos/api.secret`。完整订阅配置可能带有 provider URL，因此这些文件保持私有权限；Web 状态接口不会返回订阅链接或 API 密钥。

## 自动打包发布

GitHub Actions 每 6 小时检查 ShellCrash 和 Mihomo 的最新正式版。检测到新的版本组合后，会下载并校验官方 Mihomo x86_64/ARM64 二进制，固定 MetaCubeXD 面板提交，交叉编译管理器，构建两种架构的 FPK，再运行真实 Mihomo、管理页和 fnOS 身份网关的本机冒烟测试。成功后发布 FPK、SHA256SUMS、上游版本和构建来源信息。

自动发布只跟进上游当前稳定版，不会升级 NAS 上已安装的应用；请在 fnOS 手动安装新 FPK。GitHub 仓库只需默认的 `GITHUB_TOKEN`。也可从 Actions 手动指定 ShellCrash/Mihomo tag 和 package revision。

## 本地构建和检查

需要 Python 3、Go（`manager/go.mod` 指定版本）和 Linux amd64 的官方 `fnpack`。运行时不要求 Docker。

```sh
python3 scripts/check.py
(cd manager && go test -race ./... && go vet ./...)
python3 scripts/release.py build \
  --tag 1.9.4 \
  --mihomo-tag v1.19.31 \
  --version 1.9.4-7 \
  --fnpack /absolute/path/to/fnpack
bash scripts/smoke.sh .build/package/x86
```

FPK 和校验文件生成在 `dist/`，按架构准备的目录在 `.build/package/x86/` 和 `.build/package/arm/`。冒烟测试会启动真实 Mihomo 与静态面板；CI runner 不要求创建 TUN 设备，整机路由和真实公网请求在 fnOS NAS 上单独验证。

## 上游

- [ShellCrash Releases](https://github.com/juewuy/ShellCrash/releases)
- [Mihomo Releases](https://github.com/MetaCubeX/mihomo/releases)
- [MetaCubeXD 面板](https://github.com/MetaCubeX/metacubexd)
- [飞牛官方 fnpack 文档](https://developer.fnnas.com/docs/cli/fnpack/)
