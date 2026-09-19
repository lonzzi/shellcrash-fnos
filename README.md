# ShellCrash for 飞牛 fnOS

把 [ShellCrash](https://github.com/juewuy/ShellCrash) 官方 Docker 镜像封装为 fnOS FPK。安装后会在飞牛桌面添加图标，点击后在 fnOS 内嵌小窗口打开 Mihomo Web 面板。

这是社区封装，与 ShellCrash 作者和飞牛官方无隶属关系。ShellCrash 及其上游资源遵循各自许可证；本仓库保留上游 GPL-3.0 许可证文本。

## 安装与首次使用

1. 在 fnOS 应用中心安装并启用 Docker。
2. 从 [Releases](https://github.com/lonzzi/shellcrash-fnos/releases) 下载最新的 FPK，在应用中心选择“手动安装”。
3. 等待应用从 Docker Hub 下载官方镜像。NAS 需要联网，FPK 本身不包含容器镜像。
4. 从桌面点击 ShellCrash 图标。Web 面板在 fnOS 的内嵌小窗口打开，默认访问 /ui/。
5. 面板 API 使用安装时生成的随机密钥。首次打开若要求填写密钥，请从 /vol1/@appconf/shellcrash-fnos/api.secret 读取。

应用会生成一个空白 Mihomo 配置，让 Web 面板启动并可连接 API。它不含任何代理节点，也不会替你导入订阅。导入自己的配置后再使用代理。应用会在容器启动时启用官方 S6 服务；需要通过 ShellCrash 菜单管理配置时，可以 SSH 到 NAS 后运行：

    docker exec -it shellcrash-fnos crash

## 端口、网络和安全

- Web 面板：NAS 的 TCP 19120 → 容器 TCP 9999。桌面图标使用 iframe 类型打开。
- HTTP/SOCKS 混合代理：NAS 的 TCP/UDP 17890 → 容器 TCP/UDP 7890。
- 配置、订阅和 JSON 配置文件分别保存在 /vol1/@appdata/shellcrash-fnos/ShellCrash/configs、yamls 和 jsons。
- API 密钥保存在 /vol1/@appconf/shellcrash-fnos/api.secret，权限为 0600。ShellCrash 设置文件也会保存同一密钥，供 Mihomo API 校验。
- 默认使用普通 Docker bridge 网络和容器内代理，不开放 Docker socket，不使用 host 网络，也不修改 fnOS 宿主机防火墙。

Web 面板可以管理代理配置和连接，请限制 19120 与 17890 端口的可访问范围。不要把它们直接暴露到互联网；远程访问请使用 VPN 或带身份验证的 HTTPS 反向代理。

透明代理、旁路由和接管其他设备流量需要单独规划网络。上游 Docker 指南推荐 macvlan 等方式，并要求相应的 Linux 网络能力；本 FPK 默认不授予容器这些权限。

## 配置与升级

- 官方 ShellCrash 稳定版镜像由 Docker Hub 提供，FPK 将镜像锁定到多架构索引摘要。
- 自动升级工作流不会升级 NAS 上已经安装的应用。检查更新后请在 fnOS 手动安装新 FPK。
- FPK 更新保留配置、API 密钥、订阅和启动标记；安装脚本只在新安装时生成初始文件。
- 卸载时请在 fnOS 保留仍需使用的应用数据。
- `configs/.autostart` 会启用上游 S6 的 Mihomo 核心和启动后处理服务；请保留此文件以便容器重启后提供桌面 Web 面板。

## 自动打包

GitHub Actions 每 6 小时检查 ShellCrash 最新正式 GitHub Release，并查找对应的官方 Docker Hub 稳定标签（如 ShellCrash 1.9.4 对应 juewuy/shellcrash:1.9.4release）。

工作流会验证 amd64 和 ARM64 镜像、锁定镜像摘要、用官方 fnpack 构建 FPK，在 Linux amd64 runner 启动容器检查面板与 API 密钥，再发布 FPK、SHA256SUMS 和 upstream.json。如果上游镜像还没发布或检查失败，当前 Release 不会被替换，后续调度会再试。

只需仓库内置的 GITHUB_TOKEN，不需要个人 PAT。定时工作流仅在默认分支运行；GitHub 可能延迟执行，长期无活动的公开仓库也可能暂停定时任务。它只跟进最新正式版，不回补定时检查间错过的历史版本。

可在 Actions 手动指定 upstream_tag 和 package_revision 重打包。不要覆盖已经发布的相同版本；如需重打包同一个上游版本，请增加修订号。

## 本地构建

准备 Python 3、Docker CLI + buildx，以及[官方 fnpack](https://developer.fnnas.com/docs/cli/fnpack/)：

    python3 scripts/check.py
    python3 scripts/release.py build \
      --tag 1.9.4 \
      --image-tag 1.9.4release \
      --version 1.9.4.1 \
      --fnpack /absolute/path/to/fnpack

FPK 和校验文件生成在 dist/，构建目录在 .build/package/。运行完整 Web 面板冒烟检查还需要 Docker daemon：

    SHELLCRASH_IMAGE=juewuy/shellcrash@sha256:<官方多架构摘要> bash scripts/smoke.sh

## 上游链接

- [ShellCrash 稳定版 Releases](https://github.com/juewuy/ShellCrash/releases)
- [ShellCrash 官方 Docker 部署说明](https://github.com/juewuy/ShellCrash/blob/dev/docker/README.md)
- [ShellCrash 上游许可证](https://github.com/juewuy/ShellCrash/blob/1.9.4/LICENSE.txt)
- [飞牛 Docker 应用开发文档](https://developer.fnnas.com/docs/examples/docker/)
- [飞牛 fnpack 文档](https://developer.fnnas.com/docs/cli/fnpack/)
