import pathlib

from playwright.sync_api import sync_playwright

HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent
NAMES = ["status", "overlap", "report", "why"]

with sync_playwright() as p:
    browser = p.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1000, "height": 600}, device_scale_factor=2)
    for name in NAMES:
        page.goto((HERE / f"{name}.html").as_uri())
        page.locator("body").screenshot(path=str(OUT / f"{name}.png"))
        print(f"rendered {name}.png")
    browser.close()
