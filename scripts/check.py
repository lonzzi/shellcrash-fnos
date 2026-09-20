#!/usr/bin/env python3
"""Validate the native fnOS package and exercise install/upgrade safeguards."""
import base64
import importlib.util
import io
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import zipfile
from unittest.mock import patch

ROOT = pathlib.Path(__file__).resolve().parents[1]


def run(script, env):
    subprocess.run(["bash", str(script)], env=env, check=True)


for name in ("config/resource", "config/privilege", "app/ui/config", "wizard/install"):
    json.loads((ROOT / name).read_text(encoding="utf-8"))

manifest = (ROOT / "manifest").read_text(encoding="utf-8")
assert "appname=shellcrash-fnos\n" in manifest
assert "service_port=7890\n" in manifest
assert "platform=all\n" in manifest
port_config = (ROOT / "ShellCrash.sc").read_text(encoding="utf-8")
assert 'src.ports="7890/tcp,7890/udp"' in port_config
assert 'dst.ports="7890/tcp,7890/udp"' in port_config
privilege = json.loads((ROOT / "config/privilege").read_text(encoding="utf-8"))
assert privilege["defaults"]["run-as"] == "root"
resource = json.loads((ROOT / "config/resource").read_text(encoding="utf-8"))
assert "docker-project" not in resource
assert "data-share" in resource

desktop = json.loads((ROOT / "app/ui/config").read_text(encoding="utf-8"))[".url"]["shellcrash-fnos.main"]
assert desktop["type"] == "iframe"
assert desktop["url"] == "/app/shellcrash-fnos/manager/"
assert desktop["gatewayPrefix"] == "/app/shellcrash-fnos"
assert desktop["gatewaySocket"] == "app.sock"
assert desktop["allUsers"] is False
assert desktop["control"]["accessPerm"] == "readonly"
assert "os_min_version=1.1.3100\n" in manifest
assert not (ROOT / "app/docker").exists()
assert not (ROOT / "app/subscription-manager").exists()

main_script = (ROOT / "cmd/main").read_text(encoding="utf-8")
assert 'MANAGER="$APP_DIR/bin/shellcrash-manager"' in main_script
assert 'CORE="$APP_DIR/bin/mihomo"' in main_script
assert 'export MIXED_PORT="7890"' in main_script
assert 'DASHBOARD_DIR="$APP_DIR/dashboard"' in main_script
assert 'export CORE_CONTROLLER="127.0.0.1:9999"' in main_script
assert 'restore_old_docker' in main_script and 'remove_old_docker' in main_script
native_source = (ROOT / "manager/native.go").read_text(encoding="utf-8")
assert 'net.ParseIP(host).IsLoopback()' in native_source
assert 'strings.HasPrefix(suffix, "/ui/")' in native_source
assert 'http.FileServer(http.Dir(a.dashboardDir))' in native_source
manager_source = (ROOT / "manager/main.go").read_text(encoding="utf-8")
assert 'envOr("MIXED_PORT", "7890")' in manager_source
assert '"DASHBOARD_DIR"' in manager_source
assert '"CORE_CONTROLLER"' in manager_source
assert '"tunActive"' in manager_source
assert 'route-exclude-address' in manager_source

for script in (ROOT / "cmd").iterdir():
    if script.is_file():
        subprocess.run(["bash", "-n", str(script)], check=True)
subprocess.run(["bash", "-n", str(ROOT / "scripts/smoke.sh")], check=True)

release_spec = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
release = importlib.util.module_from_spec(release_spec)
release_spec.loader.exec_module(release)

with tempfile.TemporaryDirectory() as temporary:
    base = pathlib.Path(temporary)
    env = {
        **os.environ,
        "TRIM_PKGETC": str(base / "etc"),
        "TRIM_PKGVAR": str(base / "var"),
        "TRIM_TEMP_LOGFILE": str(base / "error"),
    }
    run(ROOT / "cmd/install_init", env)
    secret_file = base / "etc/api.secret"
    settings = base / "var/ShellCrash/configs/ShellCrash.cfg"
    profile = base / "var/ShellCrash/yamls/config.yaml"
    providers = base / "var/ShellCrash/providers"
    marker = base / "var/ShellCrash/configs/.autostart"
    secret = secret_file.read_text().strip()
    assert len(secret) == 64 and all(char in "0123456789abcdef" for char in secret)
    assert secret_file.stat().st_mode & 0o777 == 0o600
    assert f"secret={secret}\n" in settings.read_text()
    assert "mix_port=7890\n" in settings.read_text()
    assert "mixed-port: 7890\n" in profile.read_text()
    assert marker.is_file() and providers.is_dir()
    assert not (base / "var/ShellCrash/configs/command.env").exists()

    # Simulate upgrading an installation whose saved profile and ShellCrash settings use 17890.
    profile.write_text("mixed-port: 17890\nproxy-providers:\n  existing:\n    type: http\n    url: https://sub.example/nodes\n")
    settings.write_text("mix_port=17890\ndb_port=9999\nsecret=preserve-me\n")
    existing_provider = providers / "provider.yaml"
    existing_provider.write_text("proxies:\n  - name: keep\n")
    before = {
        path: path.read_bytes()
        for path in (secret_file, settings, profile, existing_provider, marker)
    }
    run(ROOT / "cmd/install_init", env)
    assert all(path.read_bytes() == content for path, content in before.items())

    old_package = base / "old-package"
    (old_package / "docker").mkdir(parents=True)
    (old_package / "docker/docker-compose.yaml").write_text("legacy Docker package marker\n")
    fake_bin = base / "bin"
    fake_bin.mkdir()
    docker_log = base / "docker.log"
    fake_docker = fake_bin / "docker"
    fake_docker.write_text(
        "#!/bin/sh\n"
        "case \"$1\" in\n"
        "  inspect) exit 0 ;;\n"
        "  stop|start|rm) printf '%s\\n' \"$*\" >> \"$DOCKER_LOG\"; exit 0 ;;\n"
        "  *) exit 1 ;;\n"
        "esac\n"
    )
    fake_docker.chmod(0o755)
    upgrade_env = {
        **env,
        "TRIM_APPDEST": str(old_package),
        "PATH": f"{fake_bin}:{os.environ.get('PATH', '')}",
        "DOCKER_LOG": str(docker_log),
    }
    settings.write_text("mix_port=17890\ndb_port=9999\nsecret=preserve-me\ndisoverride=0\n")
    manager_settings = base / "var/ShellCrash/configs/subscription-manager.json"
    manager_settings.write_text(json.dumps({"url": "https://sub.example/profile?token=private", "intervalHours": 24}))
    profile.write_text("proxy-providers:\n  primary:\n    type: http\n    url: https://sub.example/nodes\n")
    original_settings = settings.read_text()
    original_profile = profile.read_bytes()
    run(ROOT / "cmd/upgrade_init", upgrade_env)
    assert "disoverride=1\n" in settings.read_text()
    assert "mix_port=17890\n" in settings.read_text()
    assert "db_port=9999\n" in settings.read_text()
    assert "secret=preserve-me\n" in settings.read_text()
    assert profile.read_bytes() == original_profile
    config_backup = base / "var/ShellCrash/configs/fnos-subscription-shellcrash-settings.backup"
    assert config_backup.read_text() == original_settings
    assert config_backup.stat().st_mode & 0o777 == 0o600
    backups = list((base / "var/.codex-backups").glob("shellcrash-before-native-*.tar.gz"))
    assert len(backups) == 1 and backups[0].stat().st_mode & 0o777 == 0o600
    listing = subprocess.check_output(["tar", "-tzf", str(backups[0])], text=True)
    assert "ShellCrash/yamls/config.yaml" in listing and "api.secret" in listing
    assert "stop shellcrash-fnos\n" in docker_log.read_text()
    assert (base / "var/.native-upgrade-pending").is_file()

    (old_package / "bin").mkdir()
    for binary in (old_package / "bin/mihomo", old_package / "bin/shellcrash-manager"):
        binary.write_text("#!/bin/sh\nexit 0\n")
        binary.chmod(0o755)
    dashboard = old_package / "dashboard"
    dashboard.mkdir()
    (dashboard / "index.html").write_text("<html><head></head></html>")
    run(ROOT / "cmd/upgrade_callback", upgrade_env)

    release_pair = [
        {"tag_name": "1.9.4-mihomo.1.19.30-fnos.6", "draft": False},
        {"tag_name": "1.9.4-mihomo.1.19.31-fnos.7", "draft": False},
    ]
    exact, maximum = release.release_revision(release_pair, "1.9.4", "1.19.31")
    assert exact is release_pair[1] and maximum == 7

    shell_release = {"tag_name": "1.9.4", "draft": False, "prerelease": False}
    core_release = {"tag_name": "v1.19.31", "draft": False, "prerelease": False}
    latest = {"tag_name": "1.9.4-mihomo.1.19.31-fnos.7", "draft": False}
    output = base / "github-output"
    with patch.dict(
        os.environ,
        {
            "GITHUB_OUTPUT": str(output),
            "GITHUB_REPOSITORY": "owner/repo",
            "UPSTREAM_TAG": "",
            "MIHOMO_TAG": "",
            "PACKAGE_REVISION": "",
        },
    ), patch.object(release, "api", side_effect=[shell_release, core_release, release_pair, latest]):
        release.detect()
    detected = dict(line.split("=", 1) for line in output.read_text().splitlines())
    assert detected["build"] == "false"
    assert detected["tag"] == "1.9.4"
    assert detected["mihomo_tag"] == "v1.19.31"
    assert detected["release_tag"] == "1.9.4-mihomo.1.19.31-fnos.7"
    assert detected["version"] == "1.9.4-7"

    archive_buffer = io.BytesIO()
    with zipfile.ZipFile(archive_buffer, "w") as bundle:
        bundle.writestr("metacubexd-gh-pages/index.html", "<html><head></head></html>")
        bundle.writestr("metacubexd-gh-pages/_nuxt/app.js", "window.app=true")
    archive_data = archive_buffer.getvalue()

    class FakeResponse:
        def __init__(self, data):
            self.data = data

        def __enter__(self):
            return self

        def __exit__(self, *_):
            return None

        def read(self):
            return self.data

    dashboard_target = base / "dashboard"
    license_text = "MIT License\n"
    license_response = {
        "encoding": "base64",
        "content": base64.b64encode(license_text.encode()).decode(),
        "license": {"spdx_id": "MIT"},
    }
    with patch.object(release, "api", side_effect=[{"sha": "1234567890abcdef"}, license_response]), patch.object(
        release.urllib.request, "urlopen", return_value=FakeResponse(archive_data)
    ):
        dashboard_info = release.fetch_dashboard(dashboard_target)
    assert dashboard_info["commit"] == "1234567890abcdef"
    assert dashboard_info["license"] == "MIT"
    assert (dashboard_target / "index.html").is_file()
    assert (dashboard_target / "_nuxt/app.js").read_text() == "window.app=true"
    assert (dashboard_target / "THIRD-PARTY-LICENSE.txt").read_text() == license_text

    fixture_root = base / "package-source"
    fixture_root.mkdir()
    for name in ("app", "cmd", "config", "wizard"):
        shutil.copytree(ROOT / name, fixture_root / name)
    for name in ("manifest", "ICON.PNG", "ICON_256.PNG", "LICENSE-UPSTREAM.txt", "ShellCrash.sc"):
        shutil.copy2(ROOT / name, fixture_root / name)
    fake_dashboard = base / "metacubexd"
    fake_dashboard.mkdir()
    (fake_dashboard / "index.html").write_text("<html><head></head></html>")
    fake_fnpack = base / "fnpack"
    fake_fnpack.write_text("#!/bin/sh\nprintf test > fixture.fpk\n")
    fake_fnpack.chmod(0o755)

    def fake_core(_release, _architecture, destination):
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(b"mihomo-test")
        destination.chmod(0o755)
        return {"asset": "test", "asset_sha256": "a" * 64}

    def fake_manager(stage, version, architecture):
        binary = stage / "app/bin/shellcrash-manager"
        binary.parent.mkdir(parents=True, exist_ok=True)
        binary.write_bytes(f"manager-{version}-{architecture}".encode())
        binary.chmod(0o755)
        return "b" * 64

    dashboard_metadata = {"commit": "1234567890abcdef", "license": "MIT"}
    with patch.object(release, "ROOT", fixture_root), patch.object(release, "fetch_core", side_effect=fake_core), patch.object(
        release, "build_manager", side_effect=fake_manager
    ):
        stage, metadata, artifact = release.stage_package(
            "amd64", "x86", "1.9.4", "1.9.4-7", core_release,
            fake_dashboard, dashboard_metadata, str(fake_fnpack)
        )
    assert artifact.is_file()
    assert (stage / "app/bin/mihomo").is_file()
    assert (stage / "app/bin/shellcrash-manager").is_file()
    assert (stage / "app/dashboard/index.html").is_file()
    assert not (stage / "app/docker").exists()
    assert not (stage / "app/subscription-manager").exists()
    assert metadata["runtime"] == "native fnOS process; no Docker service"
    assert metadata["dashboard"] == dashboard_metadata

print("Native FPK metadata, install/upgrade preservation, release detection, dashboard pinning, and package staging checks passed.")
