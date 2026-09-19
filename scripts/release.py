#!/usr/bin/env python3
"""Resolve an official ShellCrash stable release and build its fnOS FPK."""
import argparse
import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]
UPSTREAM = "juewuy/ShellCrash"
IMAGE = "juewuy/shellcrash"
GATEWAY_IMAGE = "nginx"
GATEWAY_IMAGE_TAG = "stable-alpine"
STABLE_TAG = re.compile(r"v?([0-9]+)\.([0-9]+)\.([0-9]+)\Z")


def api(path, missing=False):
    headers = {
        "Accept": "application/vnd.github+json",
        "User-Agent": "shellcrash-fnos",
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


def detect():
    requested = os.getenv("UPSTREAM_TAG", "").strip()
    if requested and not STABLE_TAG.fullmatch(requested):
        raise ValueError("Only numeric stable release tags are supported")
    release = api(
        f"repos/{UPSTREAM}/releases/"
        + ("tags/" + requested if requested else "latest")
    )
    tag = release["tag_name"]
    if release["draft"] or release["prerelease"] or not STABLE_TAG.fullmatch(tag):
        raise ValueError("Unsupported or non-stable upstream release")

    version_tag = tag[1:] if tag.startswith("v") else tag
    revision = os.getenv("PACKAGE_REVISION", "1")
    if not re.fullmatch(r"[1-9][0-9]{0,3}", revision):
        raise ValueError("Invalid package revision")

    release_tag = f"{version_tag}-fnos.{revision}"
    existing = api(
        f"repos/{os.environ['GITHUB_REPOSITORY']}/releases/tags/{release_tag}",
        missing=True,
    )
    values = {
        "build": str(existing is None or existing["draft"]).lower(),
        "tag": tag,
        "image_tag": f"{version_tag}release",
        "release_tag": release_tag,
        "version": f"{version_tag}.{revision}",
    }
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        for key, value in values.items():
            output.write(f"{key}={value}\n")
    print(json.dumps(values, sort_keys=True))


def resolve_image(repository, image_tag):
    reference = f"{repository}:{image_tag}"
    inspection = subprocess.check_output(
        ["docker", "buildx", "imagetools", "inspect", reference],
        text=True,
    )
    match = re.search(r"^Digest:\s+(sha256:[0-9a-f]{64})\s*$", inspection, re.M)
    if not match:
        raise ValueError("Official image index digest not found")
    pinned = f"{repository}@{match.group(1)}"
    raw = subprocess.check_output(
        ["docker", "buildx", "imagetools", "inspect", "--raw", pinned],
        text=True,
    )
    index = json.loads(raw)
    platforms = {
        (
            item.get("platform", {}).get("os"),
            item.get("platform", {}).get("architecture"),
        )
        for item in index.get("manifests", [])
    }
    required = {("linux", "amd64"), ("linux", "arm64")}
    if not required <= platforms:
        raise ValueError("Official image must provide linux/amd64 and linux/arm64")
    return pinned


def build(tag, image_tag, version, fnpack):
    if not STABLE_TAG.fullmatch(tag):
        raise ValueError("Invalid upstream release tag")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+\.[1-9][0-9]{0,3}", version):
        raise ValueError("Invalid package version")

    pinned = resolve_image(IMAGE, image_tag)
    gateway_pinned = resolve_image(GATEWAY_IMAGE, GATEWAY_IMAGE_TAG)
    stage = ROOT / ".build" / "package"
    if stage.exists():
        shutil.rmtree(stage)
    stage.mkdir(parents=True)
    for name in ("app", "cmd", "config", "wizard"):
        shutil.copytree(ROOT / name, stage / name)
    for name in ("manifest", "ICON.PNG", "ICON_256.PNG", "LICENSE-UPSTREAM.txt"):
        shutil.copy2(ROOT / name, stage / name)

    manifest = stage / "manifest"
    manifest.write_text(
        re.sub(r"^version=.*$", "version=" + version, manifest.read_text(), flags=re.M)
    )
    compose = stage / "app/docker/docker-compose.yaml"
    compose.write_text(
        compose.read_text()
        .replace("@@IMAGE@@", pinned)
        .replace("@@GATEWAY_IMAGE@@", gateway_pinned)
    )
    if "@@IMAGE@@" in compose.read_text() or "@@GATEWAY_IMAGE@@" in compose.read_text():
        raise ValueError("FPK Compose file still contains unresolved image placeholders")

    provenance = {
        "upstream_repository": UPSTREAM,
        "upstream_tag": tag,
        "upstream_release": f"https://github.com/{UPSTREAM}/releases/tag/{tag}",
        "package_version": version,
        "image": pinned,
        "image_tag": image_tag,
        "gateway_image": gateway_pinned,
        "gateway_image_tag": GATEWAY_IMAGE_TAG,
        "platforms": ["linux/amd64", "linux/arm64"],
        "fnpack": "1.2.3",
    }
    (stage / "app/upstream.json").write_text(json.dumps(provenance, indent=2) + "\n")
    subprocess.run(
        [str(pathlib.Path(fnpack).resolve()), "build"],
        cwd=stage,
        check=True,
    )
    artifacts = list(stage.glob("*.fpk"))
    if len(artifacts) != 1:
        raise ValueError("fnpack did not produce exactly one FPK")

    dist = ROOT / "dist"
    dist.mkdir(exist_ok=True)
    target = dist / f"shellcrash-fnos-{version}-all.fpk"
    shutil.copy2(artifacts[0], target)
    metadata = dist / "upstream.json"
    metadata.write_text(json.dumps(provenance, indent=2) + "\n")
    (dist / "SHA256SUMS").write_text(
        "".join(
            hashlib.sha256(item.read_bytes()).hexdigest() + "  " + item.name + "\n"
            for item in (target, metadata)
        )
    )
    (dist / "release-notes.md").write_text(
        f"""ShellCrash for fnOS — {version}

上游稳定版：[{tag}](https://github.com/{UPSTREAM}/releases/tag/{tag})

- 在 fnOS 桌面图标中以嵌入式小窗口打开 ShellCrash Web 面板。
- 通过 fnOS 统一网关使用登录态；管理员首次打开自动连接本机 Mihomo，无需手动填入 API 密钥。
- Mihomo API 仍保留密钥鉴权；桌面连接由本机网关代理在服务端注入密钥，密钥不会放入浏览器 URL。
- 支持 amd64 / ARM64；安装前请启用 fnOS Docker。
- 初始配置只为 Web 面板提供可启动的空白 Mihomo 配置，不包含代理节点。
- API 密钥保存在应用配置目录 api.secret；默认面板端口 19120，代理端口 17890。
- FPK 安装和升级时会从 Docker Hub 拉取官方多架构镜像，需要 NAS 联网。
- 配置保存在应用数据目录；升级保留配置。卸载时请保留仍需使用的数据。
- 默认使用容器网络和普通 HTTP/SOCKS 代理，不修改 fnOS 宿主机防火墙。
- 桌面免密入口使用 fnOS 管理员登录态，要求系统版本不低于 1.1.3100；非管理员仍不能访问该入口。

ShellCrash 镜像固定为 {pinned}；网关镜像固定为 {gateway_pinned}。
校验文件：SHA256SUMS；构建来源：upstream.json。
"""
    )
    output_path = os.getenv("GITHUB_OUTPUT")
    if output_path:
        with open(output_path, "a", encoding="utf-8") as output:
            output.write("image=" + pinned + "\n")
            output.write("gateway_image=" + gateway_pinned + "\n")
    print(target)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("detect")
    build_parser = sub.add_parser("build")
    build_parser.add_argument("--tag", required=True)
    build_parser.add_argument("--image-tag", required=True)
    build_parser.add_argument("--version", required=True)
    build_parser.add_argument("--fnpack", required=True)
    arguments = parser.parse_args()
    if arguments.command == "detect":
        detect()
    else:
        build(arguments.tag, arguments.image_tag, arguments.version, arguments.fnpack)
