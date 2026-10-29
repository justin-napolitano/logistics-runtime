"""Real Chromium, synthetic merchant pages only; all requests intercepted."""

import html

import pytest
from playwright.sync_api import sync_playwright

from portfolio_shop.browser import Amazon
from portfolio_shop.model import Blocked, Item, Manifest
from portfolio_shop.runner import prepare


@pytest.fixture
def merchant():
    state = {"quantities": {"B000OTHER1": 3}, "title": "Example Brand Plain Crackers 8 oz", "adds": 0,
             "fresh": {"B000OTHER1": 4}, "fresh_adds": 0}
    with sync_playwright() as p:
        browser = p.chromium.launch()
        context = browser.new_context(service_workers="block")

        def route(request):
            url = request.request.url
            if url == "https://www.amazon.com/synthetic-add":
                state["adds"] += 1
                q = state["quantities"]
                q["B000TEST01"] = q.get("B000TEST01", 0) + 1
                request.fulfill(status=200, body="ok")
            elif url == "https://www.amazon.com/synthetic-fresh-add":
                state["fresh_adds"] += 1
                q = state["fresh"]
                q["B000TEST01"] = q.get("B000TEST01", 0) + 1
                request.fulfill(status=200, body="ok")
            elif url.startswith("https://www.amazon.com/cart/localmarket?"):
                rows = "".join(
                    f'<div data-itemtype="active" data-asin="{asin}">'
                    '<a class="sc-product-link sc-removed-msg-title"></a>'
                    f'<a class="sc-product-link sc-product-title" href="/dp/{asin}">{html.escape(state["title"] if asin == "B000TEST01" else "Other product")}</a>'
                    f'<button role="combobox" aria-label="{q}. Click to change quantity">{q}</button></div>'
                    for asin, q in state["fresh"].items()
                )
                request.fulfill(content_type="text/html", body=f'''<div id="sc-expanded-cart-localmarket">
                    <h1 class="sc-cart-header">Delivery from Amazon Fresh</h1>
                    <div id="sc-expanded-cart-items">{rows}</div></div>''')
            elif url.startswith("https://www.amazon.com/dp/B000TEST01?"):
                request.fulfill(content_type="text/html", body=f'''<html><head>
                    <link rel="canonical" href="https://www.amazon.com/clp/B000TEST01"></head>
                    <body><h1 id="productTitle">{html.escape(state["title"])}</h1>
                    <div id="core-dp-alm-atc-fs">
                    <button id="qs-widget-button-atfc-fresh-dp-synthetic-announce"
                      aria-label="Current quantity Qty: 1. Click to change quantity">Qty: 1</button>
                    <span><input type="submit" onclick="fetch('/synthetic-fresh-add',{{method:'POST'}})">
                    Add to cart</span></div>
                    <button onclick="throw new Error('Repeat must never be clicked')">Repeat this item</button>
                    <button onclick="throw new Error('Checkout must never be clicked')">Place your order</button>
                    </body></html>''')
            elif url == "https://www.amazon.com/gp/cart/view.html":
                rows = "".join(
                    f'<div data-itemtype="active" data-asin="{asin}">'
                    f'<span class="sc-product-title">{html.escape(state["title"] if asin == "B000TEST01" else "Other product")}</span>'
                    f'<select name="quantity"><option selected value="{q}">{q}</option></select></div>'
                    for asin, q in state["quantities"].items()
                )
                if not rows:
                    rows = "<h1>Your Amazon Cart is empty</h1>"
                request.fulfill(content_type="text/html", body=f'<div id="sc-active-cart">{rows}</div>')
            elif url == "https://www.amazon.com/dp/B000TEST01":
                request.fulfill(content_type="text/html", body=f'''<html><head>
                    <link rel="canonical" href="https://www.amazon.com/dp/B000TEST01"></head>
                    <body><h1 id="productTitle">{html.escape(state["title"])}</h1>
                    <select id="quantity"><option value="1">1</option></select>
                    <button id="add-to-cart-button" onclick="fetch('/synthetic-add',{{method:'POST'}})
                      .then(()=>document.querySelector('#ack').innerText='Added to cart')">Add to cart</button>
                    <div id="ack"></div>
                    <button onclick="throw new Error('Checkout must never be clicked')">Place your order</button>
                    </body></html>''')
            else:
                request.abort()

        context.route("**/*", route)
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        yield Amazon(page), state
        browser.close()
        assert not errors, f"Unexpected browser action: {errors}"


def test_real_browser_prepare_and_replay(merchant, tmp_path):
    amazon, state = merchant
    manifest = Manifest("amazon", (Item("B000TEST01", 2, ("Example Brand", "Plain", "8 oz")),))
    journal = tmp_path / "journal.json"
    prepare(amazon, manifest, journal)
    assert state["quantities"] == {"B000OTHER1": 3, "B000TEST01": 2}
    assert state["adds"] == 2
    prepare(amazon, manifest, journal)
    assert state["adds"] == 2


def test_real_browser_wrong_package_never_adds(merchant, tmp_path):
    amazon, state = merchant
    state["title"] = "Example Brand Plain Crackers 16 oz"
    manifest = Manifest("amazon", (Item("B000TEST01", 2, ("Example Brand", "8 oz")),))
    with pytest.raises(Blocked, match="package"):
        prepare(amazon, manifest, tmp_path / "journal.json")
    assert state["adds"] == 0


def test_fresh_cannot_accidentally_modify_retail_cart(merchant, tmp_path):
    amazon, state = merchant
    amazon.channel = "fresh"
    amazon.page.route("**/cart/localmarket?*", lambda r: r.fulfill(content_type="text/html", body="<h1>Retail cart</h1>"))
    with pytest.raises(Blocked, match="Fresh cart identity"):
        prepare(amazon, Manifest("fresh", (Item("B000TEST01", 1, ("Example",)),)), tmp_path / "journal.json")
    assert state["adds"] == 0


def test_unknown_cart_is_not_empty(merchant):
    amazon, _ = merchant
    amazon.page.route("**/gp/cart/view.html", lambda r: r.fulfill(
        content_type="text/html", body='<div id="sc-active-cart"><h1>Loading</h1></div>',
    ))
    with pytest.raises(Blocked, match="cannot assume"):
        amazon.cart()


def test_checkpoint_stops_without_clicking_human_verification(merchant):
    amazon, state = merchant
    amazon.page.route("**/checkpoint/continue", lambda r: r.fulfill(
        content_type="text/html", body='<h1>Check the box to continue</h1>'
        '<input type="checkbox" aria-label="I am human" onclick="throw new Error(\'Must be manual\')">',
    ))
    with pytest.raises(Blocked, match="human verification"):
        amazon.navigate("https://www.amazon.com/checkpoint/continue")
    assert state["adds"] == 0
    assert state["fresh_adds"] == 0


def test_fresh_browser_prepare_and_replay_preserves_both_carts(merchant, tmp_path):
    amazon, state = merchant
    amazon.channel = "fresh"
    manifest = Manifest("fresh", (Item("B000TEST01", 2, ("Example Brand", "8 oz")),))
    journal = tmp_path / "journal.json"
    prepare(amazon, manifest, journal)
    assert state["fresh"] == {"B000OTHER1": 4, "B000TEST01": 2}
    assert state["quantities"] == {"B000OTHER1": 3}
    assert state["fresh_adds"] == 2
    prepare(amazon, manifest, journal)
    assert state["fresh_adds"] == 2
