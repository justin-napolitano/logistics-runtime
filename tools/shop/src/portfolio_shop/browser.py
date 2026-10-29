from __future__ import annotations

import re
from contextlib import contextmanager
from urllib.parse import urlparse

from playwright.sync_api import Page, sync_playwright

from .model import Blocked, CartRow, Item
from .state import private_dir


ORIGIN = "https://www.amazon.com"
CART_URL = ORIGIN + "/gp/cart/view.html"
FRESH_BRAND = "QW1hem9uIEZyZXNo"
FRESH_CART_URL = ORIGIN + "/cart/localmarket?almBrandId=" + FRESH_BRAND


@contextmanager
def browser(home, headless=False):
    profile = home / "browser"
    private_dir(profile)
    with sync_playwright() as playwright:
        context = playwright.chromium.launch_persistent_context(
            str(profile), headless=headless, accept_downloads=False,
        )
        context.set_default_timeout(8000)
        try:
            yield context.pages[0] if context.pages else context.new_page()
        finally:
            context.close()


class Amazon:
    """Conservative adapters for the separately observed retail and Fresh carts."""

    def __init__(self, page: Page, channel: str = "amazon"):
        self.page = page
        self.channel = channel

    def navigate(self, url: str) -> None:
        self.page.goto(url, wait_until="domcontentloaded", timeout=30000)
        parsed = urlparse(self.page.url)
        if parsed.scheme != "https" or parsed.hostname != "www.amazon.com":
            raise Blocked("Unexpected merchant origin")
        if "/ap/" in parsed.path or self.page.locator('input[type="password"]').count():
            raise Blocked("Sign-in required: run shop login")
        if parsed.path.startswith("/checkpoint/") or self.page.locator(
            '#captchacharacters, form[action*="validateCaptcha"]'
        ).count():
            raise Blocked("Amazon requires human verification: run shop login")

    def cart(self) -> list[CartRow]:
        fresh = self.channel == "fresh"
        self.navigate(FRESH_CART_URL if fresh else CART_URL)
        if fresh:
            header = self.page.locator("#sc-expanded-cart-localmarket .sc-cart-header")
            if header.count() != 1 or "Delivery from Amazon Fresh" not in header.inner_text():
                raise Blocked("Fresh cart identity could not be verified")
        root = self.page.locator("#sc-expanded-cart-items" if fresh else "#sc-active-cart")
        root.wait_for(state="visible")
        active = root.locator('[data-itemtype="active"][data-asin]')
        if active.count() == 0:
            empty = root.get_by_text(re.compile(r"Your Amazon Cart is empty", re.I))
            if empty.count() != 1 or not empty.is_visible():
                raise Blocked("Unrecognized or loading cart; cannot assume it is empty")
            return []
        rows = []
        for row in active.all():
            asin = row.get_attribute("data-asin") or ""
            if not re.fullmatch(r"[A-Z0-9]{10}", asin):
                raise Blocked("Cart row has no recognized ASIN")
            if fresh:
                # Fresh includes empty/removed-message product links before the
                # actual title. Never mistake those for a missing product title.
                title = row.locator("a.sc-product-link.sc-product-title:visible").inner_text().strip()
                quantity = row.locator('button[role="combobox"]:visible')
                label = quantity.get_attribute("aria-label") if quantity.count() == 1 else ""
                match = re.fullmatch(r"(\d+)\. Click to change quantity", label or "")
                value = match[1] if match else ""
            else:
                title = row.locator(".sc-product-title").inner_text().strip()
                quantity = row.locator('select[name="quantity"], input[name="quantity"], input[name="quantityBox"]')
                if quantity.count() != 1:
                    raise Blocked(f"{asin}: unrecognized quantity control")
                value = quantity.input_value()
            if not title:
                raise Blocked(f"{asin}: missing visible cart title")
            if not value.isdigit() or not 1 <= int(value) <= 999:
                raise Blocked(f"{asin}: unrecognized quantity")
            if not row.is_visible():
                raise Blocked("Cart contains hidden/unrecognized rows")
            rows.append(CartRow(asin, title, int(value)))
        return rows

    def inspect(self, item: Item) -> None:
        suffix = f"?almBrandId={FRESH_BRAND}&fpw=alm" if self.channel == "fresh" else ""
        self.navigate(f"{ORIGIN}/dp/{item.asin}" + suffix)
        canonical = self.page.locator('link[rel="canonical"]')
        if canonical.count() != 1 or not re.search(
            rf"/(?:dp|clp)/{item.asin}(?:[/?]|$)", canonical.get_attribute("href") or ""
        ):
            raise Blocked(f"{item.asin}: redirected or unrecognized product identity")
        title = self.page.locator("#productTitle")
        title.wait_for(state="visible")
        item.check_title(title.inner_text())
        fresh = self.channel == "fresh"
        fresh_root = self.page.locator("#core-dp-alm-atc-fs")
        if not fresh and fresh_root.count() and fresh_root.is_visible():
            raise Blocked("Fresh purchase controls cannot be used for a retail manifest")
        button = self.page.locator(
            '#core-dp-alm-atc-fs input[type="submit"]:visible' if fresh else "#add-to-cart-button"
        )
        if button.count() != 1 or not button.is_visible() or not button.is_enabled():
            raise Blocked(f"{item.asin}: unavailable or unsupported purchase controls")
        if fresh and button.locator("..").inner_text().strip().casefold() != "add to cart":
            raise Blocked("Fresh purchase button label changed")
        if fresh:
            quantity = fresh_root.locator('button[id^="qs-widget-button-atfc-fresh-dp-"]:visible')
            if quantity.count() != 1 or quantity.get_attribute("aria-label") != "Current quantity Qty: 1. Click to change quantity":
                raise Blocked(f"{item.asin}: cannot verify a single-unit Fresh add")
        else:
            quantity = self.page.locator("select#quantity")
            if quantity.count() != 1 or quantity.input_value() != "1":
                raise Blocked(f"{item.asin}: cannot verify a single-unit add")
        # Never choose subscriptions, sellers, replacements, or repeat schedules.
        recurring = self.page.locator('input[type="radio"]:checked')
        for control in recurring.all():
            marker = (control.get_attribute("id") or "") + (control.get_attribute("value") or "")
            if re.search(r"subscribe|sns|recurr", marker, re.I):
                raise Blocked("Subscription selected; choose a one-time offer manually")

    def add(self, item: Item) -> None:
        # Explicitly no retries, checkout, Buy Now, or Place Order selectors.
        if self.channel == "fresh":
            self.page.locator('#core-dp-alm-atc-fs input[type="submit"]:visible').click(timeout=8000)
            # No guessed acknowledgement selectors or undocumented endpoints.
            # The runner verifies the whole selected cart; ambiguity stops the run.
            self.page.wait_for_timeout(1000)
            return
        self.page.locator("#add-to-cart-button").click(timeout=8000, no_wait_after=True)
        # Wait for visible acknowledgement, then verify the full cart separately.
        self.page.get_by_text(re.compile(r"^Added to (?:your )?cart$", re.I)).first.wait_for(
            state="visible", timeout=10000,
        )


def diagnose(page: Page) -> dict:
    """Visible UI structure only; no cookies, form values, tokens, HTML or screenshots."""
    return page.evaluate("""() => ({
        url: location.origin + location.pathname,
        headings: [...document.querySelectorAll('h1,h2')]
          .filter(e => e.getClientRects().length).map(e => e.innerText.slice(0,120)).slice(0,20),
        cartLinks: [...document.querySelectorAll('a')]
          .filter(e => e.getClientRects().length && /cart/i.test(e.innerText + ' ' + e.id))
          .slice(0,15).map(e => ({label:e.innerText.slice(0,80),id:e.id,
            path:new URL(e.href,location.href).pathname})),
        activeCartPresent: !!document.querySelector('#sc-active-cart'),
        activeRowCount: document.querySelectorAll('#sc-active-cart [data-itemtype="active"][data-asin]').length,
        freshCartPresent: !!document.querySelector('#sc-expanded-cart-items'),
        freshRowCount: document.querySelectorAll('#sc-expanded-cart-items [data-itemtype="active"][data-asin]').length
    })""")
