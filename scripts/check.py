#!/usr/bin/env python3
"""Check package metadata, fresh-install setup, and upgrade preservation."""
import importlib.util
import json
import os
import pathlib
import re
import subprocess
import tempfile
from unittest.mock import patch

ROOT = pathlib.Path(__file__).resolve().parents[1]

for name in ("config/resource", "config/privilege", "app/ui/config", "wizard/install"):
    json.loads((ROOT / name).read_text())

manifest = (ROOT / "manifest").read_text()
assert "appname=shellcrash-fnos\n" in manifest
assert "service_port=19120\n" in manifest
desktop = json.loads((ROOT / "app/ui/config").read_text())[".url"]["shellcrash-fnos.main"]
assert desktop["type"] == "iframe"
assert desktop["url"] == "/ui/"
assert desktop["port"] == "19120"

compose = (ROOT / "app/docker/docker-compose.yaml").read_text()
assert "@@IMAGE@@" in compose
assert "17890:7890/tcp" in compose and "17890:7890/udp" in compose
assert "contents.d/shellcrash:ro" in compose and "contents.d/afstart:ro" in compose
assert "network_mode: host" not in compose
assert "privileged: true" not in compose
assert "docker.sock" not in compose

for script in (ROOT / "cmd").iterdir():
    if script.is_file():
        subprocess.run(["bash", "-n", str(script)], check=True)
subprocess.run(["bash", "-n", str(ROOT / "scripts/smoke.sh")], check=True)

with tempfile.TemporaryDirectory() as temporary:
    base = pathlib.Path(temporary)
    env = {
        **os.environ,
        "TRIM_PKGETC": str(base / "etc"),
        "TRIM_PKGVAR": str(base / "var"),
        "TRIM_TEMP_LOGFILE": str(base / "error"),
    }
    subprocess.run(["bash", str(ROOT / "cmd/install_init")], env=env, check=True)
    secret_file = base / "etc/api.secret"
    settings = base / "var/ShellCrash/configs/ShellCrash.cfg"
    profile = base / "var/ShellCrash/yamls/config.yaml"
    command_env = base / "var/ShellCrash/configs/command.env"
    marker = base / "var/ShellCrash/configs/.autostart"
    secret = secret_file.read_text().strip()
    assert re.fullmatch(r"[0-9a-f]{64}", secret)
    assert secret_file.stat().st_mode & 0o777 == 0o600
    assert f"secret={secret}\n" in settings.read_text()
    assert "network_check=OFF\n" in settings.read_text()
    assert "proxies: []" in profile.read_text()
    assert "BINDIR='/etc/ShellCrash'" in command_env.read_text()
    assert "CrashCore -d $BINDIR -f $TMPDIR/config.yaml" in command_env.read_text()
    assert marker.is_file()

    before = {
        path: path.read_bytes()
        for path in (secret_file, settings, profile, command_env, marker)
    }
    subprocess.run(["bash", str(ROOT / "cmd/install_init")], env=env, check=True)
    assert all(path.read_bytes() == content for path, content in before.items())
    subprocess.run(["bash", str(ROOT / "cmd/install_callback")], env=env, check=True)
    subprocess.run(["bash", str(ROOT / "cmd/upgrade_init")], env=env, check=True)
    subprocess.run(["bash", str(ROOT / "cmd/upgrade_callback")], env=env, check=True)
    assert all(path.read_bytes() == content for path, content in before.items())

    disabled = base / "autostart-disabled"
    disabled_env = {
        **env,
        "TRIM_PKGETC": str(disabled / "etc"),
        "TRIM_PKGVAR": str(disabled / "var"),
        "TRIM_TEMP_LOGFILE": str(disabled / "error"),
    }
    subprocess.run(["bash", str(ROOT / "cmd/install_init")], env=disabled_env, check=True)
    disabled_marker = disabled / "var/ShellCrash/configs/.autostart"
    disabled_marker.unlink()
    subprocess.run(["bash", str(ROOT / "cmd/install_init")], env=disabled_env, check=True)
    assert not disabled_marker.exists()
    subprocess.run(["bash", str(ROOT / "cmd/upgrade_init")], env=disabled_env, check=True)
    subprocess.run(["bash", str(ROOT / "cmd/upgrade_callback")], env=disabled_env, check=True)
    assert not disabled_marker.exists()

    module_spec = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
    release_module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(release_module)
    release = {"tag_name": "1.9.4", "draft": False, "prerelease": False}
    existing_values = [(None, "true"), ({"draft": False}, "false"), ({"draft": True}, "true")]
    for existing, expected in existing_values:
        output = base / "github-output"
        output.write_text("")
        with patch.dict(
            os.environ,
            {
                "GITHUB_OUTPUT": str(output),
                "GITHUB_REPOSITORY": "owner/repo",
                "UPSTREAM_TAG": "",
                "PACKAGE_REVISION": "1",
            },
        ), patch.object(release_module, "api", side_effect=[release, existing]):
            release_module.detect()
        contents = output.read_text()
        assert f"build={expected}\n" in contents
        assert "image_tag=1.9.4release\n" in contents
        assert "version=1.9.4.1\n" in contents

    for unsafe in ("../main", "1.9.4-beta", "$(touch nope)"):
        with patch.dict(os.environ, {"UPSTREAM_TAG": unsafe}):
            try:
                release_module.detect()
            except ValueError:
                pass
            else:
                raise AssertionError(f"Unsafe release tag accepted: {unsafe}")

print("Package source checks passed")
