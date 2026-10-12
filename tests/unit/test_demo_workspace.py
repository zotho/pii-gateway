import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from demo.workspace import Workspace  # noqa: E402


@pytest.fixture()
def ws(tmp_path):
    w = Workspace(tmp_path / "ws")
    w.seed()
    (tmp_path / "secret.txt").write_text("outside")
    return w


def test_read_inside(ws):
    assert "Николай Петров" in ws.read("CLAUDE.md")
    assert "Ярослав" in ws.read("src/billing.py")


@pytest.mark.parametrize("p", ["../secret.txt", "src/../../secret.txt", "/etc/passwd", "..", "a/../../.."])
def test_traversal_blocked(ws, p):
    assert ws.read(p).startswith("error:")
    assert ws.ls(p).startswith("error:")
    assert ws.write(p, "x").startswith("error:")
    assert ws.grep(".", p).startswith("error:")
    assert not (ws.root.parent / "x").exists()


def test_symlink_escape_blocked(ws):
    (ws.root / "link").symlink_to(ws.root.parent / "secret.txt")
    assert ws.read("link").startswith("error:")
    (ws.root / "dlink").symlink_to(ws.root.parent, target_is_directory=True)
    assert ws.ls("dlink").startswith("error:")
    assert ws.write("dlink/new.txt", "x").startswith("error:")


def test_write_and_grep(ws):
    assert ws.write("notes/a.txt", "hello world").startswith("wrote")
    assert "notes/a.txt:1:hello world" in ws.grep("hello")
    assert "src/" in ws.ls(".")
    assert ws.call("nope", {}).startswith("error:")


def test_demo_leak_badge_matches_on_word_boundaries():
    """demo/static/app.js boundaryRe mirrors litellm/pii_leak.py: whole tokens, case-insensitive, Unicode."""
    import re
    import shutil
    import subprocess
    from pathlib import Path

    import pytest
    node = shutil.which("node")
    if not node:
        pytest.skip("node not installed")
    src = (Path(__file__).resolve().parents[2] / "demo/static/app.js").read_text(encoding="utf-8")
    fn = re.search(r"function boundaryRe[\s\S]*?\n}\n", src).group(0)
    js = fn + """
const t = (r, h) => boundaryRe(r).test(h);
console.log(JSON.stringify([t("Ivan","Ivanov"), t("Ivan","hi IVAN,"), t("contact","contacts"), t("contact","The Contact is"),
  t("Контакт","контакт!"), t("Иван","Иванов"), t("a@b.com","xa@b.com"), t("a@b.com","mail A@B.com."),
  t("+7 999 123-45-67","x+7 999 123-45-67"), t("+7 999 123-45-67","call +7 999 123-45-67!")]));"""
    out = subprocess.run([node, "-e", js], capture_output=True, text=True, check=True).stdout
    assert out.strip() == "[false,true,false,true,true,false,false,true,false,true]"
