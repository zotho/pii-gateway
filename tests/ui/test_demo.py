"""Playwright E2E for the demo app (mock mode). Needs the mock stack up (scripts/up.sh)."""
import socket
import subprocess
import sys
import time
from pathlib import Path

import httpx
import pytest
from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parents[2]
SHOTS = ROOT / ".run" / "ui-shots"
Q = "Who is the contact and who maintains billing?"


@pytest.fixture(scope="module")
def server():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    log = open(ROOT / ".run" / "logs" / "demo-test.log", "w")
    proc = subprocess.Popen([sys.executable, str(ROOT / "demo" / "server.py"), "--mode", "mock", "--port", str(port)],
                            cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
    url = f"http://127.0.0.1:{port}"
    try:
        for _ in range(60):
            try:
                if httpx.get(url + "/api/config", timeout=1).status_code == 200:
                    break
            except httpx.HTTPError:
                time.sleep(0.5)
        else:
            raise RuntimeError("demo server did not start")
        yield url
    finally:
        proc.terminate()
        try:
            proc.wait(10)
        except subprocess.TimeoutExpired:
            proc.kill()


@pytest.fixture(scope="module")
def page(server):
    SHOTS.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as p:
        b = p.chromium.launch()
        ctx = b.new_context(viewport={"width": 1440, "height": 900}, color_scheme="light")
        pg = ctx.new_page()
        pg.goto(server)
        expect(pg.locator("#sid")).not_to_have_text("-")
        pg.fill("#msg", Q)
        pg.click("#send")
        expect(pg.locator("#chat .msg.assistant")).to_have_count(1, timeout=60000)
        expect(pg.locator("#send")).to_be_enabled(timeout=30000)
        expect(pg.locator("#map tbody tr").first).to_be_visible(timeout=15000)
        expect(pg.locator("#reqs .req")).to_have_count(3, timeout=15000)
        pg.wait_for_timeout(1500)
        yield pg
        b.close()


def test_final_text_real_names(page):
    txt = page.locator("#chat .msg.assistant").inner_text()
    assert "Николай Петров" in txt and "Ярослав Кузнецов" in txt
    assert "<PERSON_" not in txt


def test_mapping_rows(page):
    rows = page.locator("#map tbody tr")
    for real in ("Николай Петров", "Ярослав Кузнецов", "nikolai.petrov@example.com"):
        row = rows.filter(has_text=real)
        assert row.count() == 1, real
        assert "<" in row.inner_text()
    assert "<PERSON_" in rows.filter(has_text="Николай Петров").inner_text()


def test_provider_panel(page):
    reqs = page.locator("#reqs .req")
    assert reqs.count() >= 3
    reqs.last.click()
    payload = page.locator("#payload").inner_text()
    assert "<PERSON_" in payload
    for bad in ("Николай", "Петров", "Кузнецов", "nikolai.petrov"):
        assert bad not in payload
    expect(page.locator("#leak")).to_have_text("leaks 0")
    assert page.locator("#leak").get_attribute("data-count") == "0"


def test_audit_tail(page):
    expect(page.locator("#audit .ev").filter(has_text="mapping.revealed").first).to_be_visible()


def test_tool_collapsed_then_expands(page):
    tool = page.locator("#chat .tool.call").first
    assert "open" not in (tool.get_attribute("class") or "")
    assert not tool.locator(".body").is_visible()
    tool.locator(".head").click()
    expect(tool.locator(".body")).to_be_visible()
    tool.locator(".head").click()
    expect(tool.locator(".body")).to_be_hidden()


def test_hover_cross_highlight(page):
    page.locator("#reqs .req").last.click()
    row = page.locator("#map tbody tr").filter(has_text="Николай Петров")
    assert page.locator("#chat .chip.hl, #payload .chip.hl").count() == 0
    row.hover()
    assert page.locator("#chat .chip.hl").count() > 0
    assert page.locator("#payload .chip.hl").count() > 0
    page.mouse.move(5, 5)
    assert page.locator("#chat .chip.hl").count() == 0


@pytest.mark.parametrize("w,h", [(1440, 900), (1280, 800)])
def test_no_page_scroll(page, w, h):
    page.set_viewport_size({"width": w, "height": h})
    page.wait_for_timeout(200)
    sh, ih = page.evaluate("[document.scrollingElement.scrollHeight, window.innerHeight]")
    sw, iw = page.evaluate("[document.scrollingElement.scrollWidth, window.innerWidth]")
    assert sh <= ih and sw <= iw
    page.set_viewport_size({"width": 1440, "height": 900})


def test_screenshots(page):
    page.locator("#reqs .req").last.click()
    page.locator("#chat .tool.call").first.locator(".head").click()
    for scheme in ("light", "dark"):
        page.emulate_media(color_scheme=scheme)
        page.wait_for_timeout(200)
        page.screenshot(path=str(SHOTS / f"demo-{scheme}.png"))
    page.set_viewport_size({"width": 1280, "height": 800})
    page.screenshot(path=str(SHOTS / "demo-dark-1280.png"))
    page.emulate_media(color_scheme="light")
    page.screenshot(path=str(SHOTS / "demo-light-1280.png"))
    page.set_viewport_size({"width": 1440, "height": 900})
    assert (SHOTS / "demo-dark.png").exists()
