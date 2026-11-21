# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Takes the screenshots for docs/tour.md: drives the local web UI in a
headless browser through the steps the tour describes. make tour runs it in
a Playwright container on the dev stack's network.

Usage: tour.py <web UI base URL> <space id> <output folder>

TOUR_USER picks the user (default alice; the space must be theirs).
TOUR_ONLY=sessions takes only the interactive session's screenshots (13-15).
"""

import os
import re
import sys
import time

from playwright.sync_api import expect, sync_playwright

BASE, SPACE, OUT = sys.argv[1].rstrip("/"), sys.argv[2], sys.argv[3]
USER = os.environ.get("TOUR_USER", "alice")
NB = f"{BASE}/#/{USER}/{SPACE}/f/sales_report.ipynb"


def shot(page, name, full=False):
    time.sleep(0.4)  # let transitions settle
    page.screenshot(path=f"{OUT}/{name}.png", full_page=full)
    print("wrote", name)


def main():
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1400, "height": 860}, color_scheme="dark")
        try:
            page.on("dialog", lambda d: d.accept())  # "leave with unsaved changes?", "end the session?"
            if os.environ.get("TOUR_ONLY") != "sessions":
                steps(page)
            sessions(page)
        except Exception:
            page.screenshot(path=f"{OUT}/failed.png")
            raise
        browser.close()


def steps(page):
    if True:
        # 1. Spaces.
        page.goto(f"{BASE}/")
        expect(page.locator("main").get_by_text("sales analysis").first).to_be_visible()
        shot(page, "01-spaces")

        # 2. One space: its notebooks and files.
        page.goto(f"{BASE}/#/spaces/{SPACE}")
        expect(page.locator("main").get_by_text("sales_report.ipynb").first).to_be_visible(timeout=15_000)
        shot(page, "02-space")

        # 3. The notebook editor.
        page.goto(NB)
        page.locator(".cm-content:visible").first.wait_for()
        expect(page.get_by_role("button", name="Run", exact=True)).to_be_visible()
        shot(page, "03-editor")

        # 4. Completion from the runner image's packages: type in the
        # third cell (the code that reads the CSV), then undo.
        cell = page.locator(".cm-content").nth(3)
        cell.click()
        page.keyboard.press("Control+End")
        page.keyboard.type("\npd.read_")
        page.wait_for_selector(".cm-tooltip-autocomplete")
        shot(page, "04-completion")
        page.keyboard.press("Escape")

        # 5. Hover documentation, on the read_csv the cell already calls
        # (the typed line undone first).
        for _ in range(12):
            page.keyboard.press("Control+z")
        page.keyboard.press("Escape")
        cell.locator(".cm-line", has_text="pd.read_csv").first.get_by_text("read_csv").hover()
        page.wait_for_selector(".cm-tooltip-hover")
        shot(page, "05-hover")
        page.mouse.move(5, 5)

        # 6. A run with parameters, followed live.
        page.goto(NB)
        page.locator(".cm-content:visible").first.wait_for()
        page.get_by_role("textbox", name="Parameters (JSON)").fill('{"region": "bandung", "months": 4}')
        page.get_by_role("button", name="Run", exact=True).click()
        page.wait_for_url(re.compile(r".*/r/[0-9a-f-]+$"))
        expect(page.get_by_text("Executing Cell 2").first).to_be_visible(timeout=60_000)
        shot(page, "06-run-live")
        expect(page.get_by_text("run finished: succeeded")).to_be_visible(timeout=120_000)
        run_url = page.url
        expect(page.get_by_text("Returned value")).to_be_visible()
        shot(page, "07-run-result", full=True)

        # 7. The rendered notebook (served sandboxed: no scripts run).
        href = page.get_by_role("link", name=re.compile("View rendered notebook")).get_attribute("href")
        page.goto(BASE + href)
        page.wait_for_load_state("networkidle")
        shot(page, "08-rendered")

        # 8. Publish the notebook as a function.
        page.goto(NB)
        page.locator(".cm-content:visible").first.wait_for()
        page.get_by_role("button", name="More actions").click()
        page.get_by_role("menuitem", name=re.compile("Publish as function")).click()
        page.get_by_label("data/sales.csv").check()
        shot(page, "09-publish")
        page.get_by_role("button", name="Publish", exact=True).click()

        # 9. The function's page: a form from its contract, and how to
        # call it from a service.
        expect(page.get_by_text("Try it")).to_be_visible(timeout=15_000)
        shot(page, "10-function")

        # 10. Call it from the form.
        page.locator("main select").first.select_option("surabaya")
        page.get_by_placeholder("integer").fill("3")
        page.get_by_role("button", name="Call", exact=True).click()
        expect(page.get_by_text("succeeded").first).to_be_visible(timeout=120_000)
        shot(page, "11-function-call", full=True)

        # 11. The space's runs and calls.
        page.goto(run_url)
        page.get_by_role("button", name=re.compile("^Runs")).first.click()
        expect(page.get_by_role("tab", name="Runs")).to_be_visible()
        shot(page, "12-runs")


def sessions(page):
    """The notebook's live kernel: cells run one by one, completion from
    the real objects, the variables panel. Ends the session at the end."""
    page.goto(NB)
    page.locator(".cm-content:visible").first.wait_for()
    bar = page.get_by_role("group", name="Interactive session")
    bar.get_by_role("button", name="Start session").click()
    expect(bar.get_by_role("button", name="Run all")).to_be_visible(timeout=180_000)

    # 13. Every cell run in the session, then the reading cell again
    # (Shift-Enter) ending with the DataFrame: shown as a table.
    bar.get_by_role("button", name="Run all").click()
    cell = page.locator(".cm-content", has_text="pd.read_csv")  # the cell that reads the CSV
    expect(page.get_by_text("In [3]:")).to_be_visible(timeout=120_000)  # the last cell ran
    expect(bar.get_by_role("button", name="Interrupt")).to_be_disabled(timeout=60_000)
    cell.click()
    page.keyboard.press("ControlOrMeta+End")
    page.keyboard.type("\nmine")
    page.keyboard.press("Escape")  # the completion list
    page.keyboard.press("Shift+Enter")
    # HTML outputs are shown in a sandboxed frame.
    expect(page.locator(".outputs iframe").first).to_be_visible(timeout=60_000)
    cell.evaluate("e => e.scrollIntoView({block: 'start'})")
    page.mouse.wheel(0, -90)
    shot(page, "13-session")

    # 14. Completion from the live DataFrame: its columns.
    cell.click()
    page.keyboard.press("ControlOrMeta+End")
    page.keyboard.type('\nmine["')
    page.wait_for_selector(".cm-tooltip-autocomplete")
    shot(page, "14-session-completion")
    page.keyboard.press("Escape")
    for _ in range(12):
        page.keyboard.press("ControlOrMeta+z")

    # 15. The variables panel.
    bar.get_by_role("button", name="Variables").click()
    expect(page.locator(".vars-panel table")).to_be_visible(timeout=15_000)
    page.locator(".session-bar").scroll_into_view_if_needed()
    shot(page, "15-variables")

    bar.get_by_role("button", name="End session").click()
    expect(bar.get_by_text("Session ended")).to_be_visible(timeout=60_000)


if __name__ == "__main__":
    main()
