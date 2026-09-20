#!/usr/bin/env python3
"""Build native fnOS FPKs from official ShellCrash and Mihomo releases."""

import argparse
import base64
import gzip
import hashlib
import io
import json
import os
import pathlib
import posixpath
import re
import shutil
import struct
import subprocess
import urllib.error
import urllib.request
import zipfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
SHELLCRASH_REPO = "juewuy/ShellCrash"
MIHOMO_REPO = "MetaCubeX/mihomo"
METACUBEXD_REPO = "MetaCubeX/metacubexd"
STABLE_TAG = re.compile(r"v?([0-9]+)\.([0-9]+)\.([0-9]+)\Z")


def version_from_tag(tag):
    match = STABLE_TAG.fullmatch(tag)
    if match is None:
        raise ValueError(f"Unsupported release tag: {tag}")
    return ".".join(match.groups())


def api(path, missing=False):
    headers = {
        "Accept": "application/vnd.github+json",
        "User-Agent": "shellcrash-fnos-native",
    }
    if os.getenv("GH_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GH_TOKEN"]
    request = urllib.request.Request("https://api.github.com/" + path, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        if missing and error.code == 404:
            return None
        raise


def stable_release(repository, requested_tag):
    if requested_tag:
        release = api(f"repos/{repository}/releases/tags/{requested_tag}")
    else:
        release = api(f"repos/{repository}/releases/latest")
    tag = release["tag_name"]
    if release["draft"] or release["prerelease"] or not STABLE_TAG.fullmatch(tag):
        raise ValueError(f"Unsupported or non-stable release for {repository}: {tag}")
    return release


def release_revision(releases, shell_version, core_version):
    maximum = 0
    pair = re.compile(
        rf"^{re.escape(shell_version)}-mihomo\.([0-9]+\.[0-9]+\.[0-9]+)-fnos\.([1-9][0-9]{{0,3}})$"
    )
    legacy = re.compile(rf"^{re.escape(shell_version)}-fnos\.([1-9][0-9]*)$")
    exact = None
    for release in releases:
        tag = release.get("tag_name", "")
        match = pair.fullmatch(tag)
        if match:
            revision = int(match.group(2))
            maximum = max(maximum, revision)
            if match.group(1) == core_version:
                exact = release
            continue
        match = legacy.fullmatch(tag)
        if match:
            maximum = max(maximum, int(match.group(1)))
    return exact, maximum


def detect():
    requested_shell = os.getenv("UPSTREAM_TAG", "").strip()
    requested_core = os.getenv("MIHOMO_TAG", "").strip()
    shell_release = stable_release(SHELLCRASH_REPO, requested_shell)
    core_release = stable_release(MIHOMO_REPO, requested_core)
    shell_tag = shell_release["tag_name"]
    core_tag = core_release["tag_name"]
    shell_version = version_from_tag(shell_tag)
    core_version = version_from_tag(core_tag)

    repository = os.environ["GITHUB_REPOSITORY"]
    releases = api(f"repos/{repository}/releases?per_page=100")
    exact, maximum = release_revision(releases, shell_version, core_version)
    requested_revision = os.getenv("PACKAGE_REVISION", "").strip()
    if requested_revision and not re.fullmatch(r"[1-9][0-9]{0,3}", requested_revision):
        raise ValueError("Invalid package revision")
    revision = int(requested_revision) if requested_revision else (maximum + 1 if exact is None else int(re.search(r"-fnos\.([0-9]+)$", exact["tag_name"]).group(1)))
    release_tag = f"{shell_version}-mihomo.{core_version}-fnos.{revision}"
    existing = api(f"repos/{repository}/releases/tags/{release_tag}", missing=True)
    if requested_revision:
        build = existing is None or existing["draft"]
    else:
        build = exact is None or exact["draft"]
        if exact is not None:
            release_tag = exact["tag_name"]
    values = {
        "build": str(build).lower(),
        "tag": shell_tag,
        "mihomo_tag": core_tag,
        "release_tag": release_tag,
        "version": f"{shell_version}-{revision}",
    }
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        for key, value in values.items():
            output.write(f"{key}={value}\n")
    print(json.dumps(values, sort_keys=True))


def asset_for(release, name):
    for asset in release.get("assets", []):
        if asset["name"] == name:
            return asset
    raise ValueError(f"Official Mihomo release is missing asset: {name}")


def download_asset(asset, destination):
    request = urllib.request.Request(
        asset["browser_download_url"],
        headers={"User-Agent": "shellcrash-fnos-native"},
    )
    with urllib.request.urlopen(request, timeout=180) as response:
        payload = response.read()
    expected = asset.get("digest", "")
    actual = "sha256:" + hashlib.sha256(payload).hexdigest()
    if expected and actual != expected:
        raise ValueError(f"Official asset digest mismatch for {asset['name']}")
    destination.write_bytes(payload)
    return actual.removeprefix("sha256:")


def binary_arch(binary):
    if len(binary) < 20 or binary[:4] != b"\x7fELF":
        raise ValueError("Mihomo release asset is not an ELF executable")
    machine = struct.unpack_from("<H", binary, 18)[0]
    return {62: "amd64", 183: "arm64"}.get(machine, "unknown")


def fetch_core(release, architecture, destination):
    version = version_from_tag(release["tag_name"])
    if architecture == "amd64":
        asset_name = f"mihomo-linux-amd64-compatible-v{version}.gz"
    elif architecture == "arm64":
        asset_name = f"mihomo-linux-arm64-v{version}.gz"
    else:
        raise ValueError(f"Unsupported package architecture: {architecture}")
    asset = asset_for(release, asset_name)
    destination.parent.mkdir(parents=True, exist_ok=True)
    compressed_path = destination.with_suffix(".gz")
    asset_sha256 = download_asset(asset, compressed_path)
    binary = gzip.decompress(compressed_path.read_bytes())
    compressed_path.unlink()
    if binary_arch(binary) != architecture:
        raise ValueError(f"Mihomo asset architecture does not match {architecture}")
    destination.write_bytes(binary)
    destination.chmod(0o755)
    return {
        "asset": asset_name,
        "asset_sha256": asset_sha256,
        "binary_sha256": hashlib.sha256(binary).hexdigest(),
        "asset_url": asset["browser_download_url"],
        "bytes": len(binary),
    }


def fetch_dashboard(destination):
    branch_commit = api(f"repos/{METACUBEXD_REPO}/commits/gh-pages")["sha"]
    archive_url = f"https://codeload.github.com/{METACUBEXD_REPO}/zip/{branch_commit}"
    request = urllib.request.Request(archive_url, headers={"User-Agent": "shellcrash-fnos-native"})
    with urllib.request.urlopen(request, timeout=180) as response:
        archive = response.read()
    archive_sha256 = hashlib.sha256(archive).hexdigest()
    destination.mkdir(parents=True, exist_ok=True)
    extracted = 0
    with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
        roots = {item.filename.split("/", 1)[0] for item in bundle.infolist() if "/" in item.filename}
        if len(roots) != 1:
            raise ValueError("MetaCubeXD archive has an unexpected directory layout")
        prefix = next(iter(roots)) + "/"
        for item in bundle.infolist():
            if item.is_dir() or not item.filename.startswith(prefix):
                continue
            relative = item.filename[len(prefix):]
            if not relative or posixpath.isabs(relative) or ".." in pathlib.PurePosixPath(relative).parts:
                raise ValueError("MetaCubeXD archive contains an unsafe file path")
            target = destination.joinpath(*pathlib.PurePosixPath(relative).parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(bundle.read(item))
            extracted += 1
    index = destination / "index.html"
    if not index.is_file() or extracted == 0:
        raise ValueError("MetaCubeXD gh-pages archive is missing index.html")

    license_document = api(f"repos/{METACUBEXD_REPO}/license")
    if license_document.get("encoding") != "base64":
        raise ValueError("MetaCubeXD license response could not be decoded")
    license_text = base64.b64decode(license_document["content"]).decode("utf-8")
    license_path = destination / "THIRD-PARTY-LICENSE.txt"
    license_path.write_text(license_text, encoding="utf-8")
    return {
        "repository": METACUBEXD_REPO,
        "branch": "gh-pages",
        "commit": branch_commit,
        "archive_url": archive_url,
        "archive_sha256": archive_sha256,
        "license": license_document.get("license", {}).get("spdx_id", "unknown"),
        "license_sha256": hashlib.sha256(license_text.encode("utf-8")).hexdigest(),
        "files": extracted,
    }


def build_manager(stage, version, architecture):
    binary = stage / "app/bin/shellcrash-manager"
    binary.parent.mkdir(parents=True, exist_ok=True)
    environment = os.environ.copy()
    environment.update({"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": architecture})
    subprocess.run(
        [
            "go",
            "build",
            "-buildvcs=false",
            "-trimpath",
            "-ldflags",
            f"-s -w -X main.buildVersion={version}",
            "-o",
            str(binary),
            ".",
        ],
        cwd=ROOT / "manager",
        env=environment,
        check=True,
    )
    binary.chmod(0o755)
    return hashlib.sha256(binary.read_bytes()).hexdigest()


def stage_package(architecture, platform, shell_version, package_version, core_release, dashboard_source, dashboard_info, fnpack):
    stage = ROOT / ".build" / "package" / platform
    if stage.exists():
        shutil.rmtree(stage)
    stage.mkdir(parents=True)
    for name in ("app", "cmd", "config", "wizard"):
        shutil.copytree(ROOT / name, stage / name)
    for name in ("manifest", "ICON.PNG", "ICON_256.PNG", "LICENSE-UPSTREAM.txt", "ShellCrash.sc"):
        shutil.copy2(ROOT / name, stage / name)
    shutil.copytree(dashboard_source, stage / "app/dashboard")

    manifest = stage / "manifest"
    manifest_text = manifest.read_text()
    manifest_text = re.sub(r"^version=.*$", "version=" + package_version, manifest_text, flags=re.M)
    manifest_text = re.sub(r"^platform=.*$", "platform=" + platform, manifest_text, flags=re.M)
    manifest.write_text(manifest_text)

    core_info = fetch_core(core_release, architecture, stage / "app/bin/mihomo")
    manager_sha256 = build_manager(stage, package_version, architecture)
    upstream = {
        "upstream_repository": SHELLCRASH_REPO,
        "upstream_tag": shell_version,
        "upstream_release": f"https://github.com/{SHELLCRASH_REPO}/releases/tag/{shell_version}",
        "core_repository": MIHOMO_REPO,
        "core_tag": core_release["tag_name"],
        "core_release": f"https://github.com/{MIHOMO_REPO}/releases/tag/{core_release['tag_name']}",
        "package_version": package_version,
        "package_platform": platform,
        "architecture": architecture,
        "core": core_info,
        "dashboard": dashboard_info,
        "manager_sha256": manager_sha256,
        "fnpack": "1.2.3",
        "runtime": "native fnOS process; no Docker service",
    }
    (stage / "app/upstream.json").write_text(json.dumps(upstream, indent=2) + "\n")
    subprocess.run([str(pathlib.Path(fnpack).resolve()), "build"], cwd=stage, check=True)
    artifacts = list(stage.glob("*.fpk"))
    if len(artifacts) != 1:
        raise ValueError(f"fnpack did not produce exactly one FPK for {platform}")
    return stage, upstream, artifacts[0]


def build(tag, mihomo_tag, version, fnpack):
    if not STABLE_TAG.fullmatch(tag) or not STABLE_TAG.fullmatch(mihomo_tag):
        raise ValueError("Invalid upstream release tag")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+-[1-9][0-9]{0,3}", version):
        raise ValueError("Invalid package version")
    shell_release = stable_release(SHELLCRASH_REPO, tag)
    core_release = stable_release(MIHOMO_REPO, mihomo_tag)
    shell_version = version_from_tag(shell_release["tag_name"])
    if not version.startswith(shell_version + "-"):
        raise ValueError("Package version does not match the ShellCrash release")

    stage_root = ROOT / ".build" / "package"
    if stage_root.exists():
        shutil.rmtree(stage_root)
    dashboard_source = ROOT / ".build" / "metacubexd"
    if dashboard_source.exists():
        shutil.rmtree(dashboard_source)
    dashboard_info = fetch_dashboard(dashboard_source)
    dist = ROOT / "dist"
    dist.mkdir(exist_ok=True)

    outputs = []
    provenance = {
        "upstream_repository": SHELLCRASH_REPO,
        "upstream_tag": shell_release["tag_name"],
        "core_repository": MIHOMO_REPO,
        "core_tag": core_release["tag_name"],
        "package_version": version,
        "packages": {},
    }
    for architecture, platform in (("amd64", "x86"), ("arm64", "arm")):
        stage, arch_upstream, artifact = stage_package(
            architecture, platform, shell_release["tag_name"], version, core_release,
            dashboard_source, dashboard_info, fnpack
        )
        target = dist / f"shellcrash-fnos-{version}-{platform}.fpk"
        shutil.copy2(artifact, target)
        outputs.append(target)
        provenance["packages"][platform] = arch_upstream

    metadata = dist / "upstream.json"
    metadata.write_text(json.dumps(provenance, indent=2) + "\n")
    all_files = outputs + [metadata]
    (dist / "SHA256SUMS").write_text(
        "".join(hashlib.sha256(item.read_bytes()).hexdigest() + "  " + item.name + "\n" for item in all_files)
    )
    shell_tag = shell_release["tag_name"]
    core_tag = core_release["tag_name"]
    (dist / "release-notes.md").write_text(
        f"""ShellCrash for fnOS — {version}

基于 ShellCrash [{shell_tag}](https://github.com/{SHELLCRASH_REPO}/releases/tag/{shell_tag}) 配置布局，使用官方 Mihomo 核心 [{core_tag}](https://github.com/{MIHOMO_REPO}/releases/tag/{core_tag})。

- 原生 fnOS 服务进程，不要求安装 Docker；FPK 按 x86_64 与 ARM64 分别打包。
- 桌面图标仍在 fnOS 内嵌小窗口打开订阅管理；使用 fnOS 管理员登录态，不单独暴露管理口。
- 迁移保留现有 profile、订阅、providers、策略组、DNS/TUN 覆盖和 API 密钥；迁移前自动生成权限为 600 的配置归档。
- 启用 TUN 后在 fnOS 宿主机网络空间启用 `auto-route` 与 Linux `auto-redirect`，并绕过本机环回、私网和链路本地网段。
- 状态页分别显示 Mihomo Core 连接状态、宿主机 TUN 接口和混合代理端口。
- 高级面板静态文件由 MetaCubeXD [{dashboard_info['commit'][:12]}](https://github.com/{METACUBEXD_REPO}/tree/{dashboard_info['commit']}) 提供，并随 FPK 一起离线安装。
- 局域网 HTTP/SOCKS 混合代理默认使用 TCP/UDP 7890；Core 控制器仅绑定 `127.0.0.1:9999`。
- 订阅 providers、组和规则不被拆分或自动改写；DNS/TUN 开关仍可分别覆盖。

架构包：`shellcrash-fnos-{version}-x86.fpk`、`shellcrash-fnos-{version}-arm.fpk`。
校验文件：SHA256SUMS；上游和二进制校验信息：upstream.json。
"""
    )
    output_path = os.getenv("GITHUB_OUTPUT")
    if output_path:
        with open(output_path, "a", encoding="utf-8") as output:
            output.write("mihomo_tag=" + core_tag + "\n")
    print("\n".join(str(item) for item in outputs))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("detect")
    build_parser = sub.add_parser("build")
    build_parser.add_argument("--tag", required=True)
    build_parser.add_argument("--mihomo-tag", required=True)
    build_parser.add_argument("--version", required=True)
    build_parser.add_argument("--fnpack", required=True)
    args = parser.parse_args()
    if args.command == "detect":
        detect()
    else:
        build(args.tag, args.mihomo_tag, args.version, args.fnpack)
