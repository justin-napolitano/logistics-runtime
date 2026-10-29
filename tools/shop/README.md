# Portable shopping worker

Python + Playwright cart preparation with manual checkout. No AI calls, AppleScript,
screen coordinates, purchase submission, schedules or subscriptions. The Go service
remains the backend; this optional browser worker never changes physical inventory.

Retail and Fresh use separate cart adapters. Their controls were inspected read-only
in a signed-in browser. Mutations are tested against synthetic pages in Chromium;
**a live signed-in preparation run has not been performed**. Amazon layout changes,
stock changes and account checks can stop a run. This is not an Amazon API.

## Install and login

Use Python 3.11+ and [uv](https://docs.astral.sh/uv/). From this directory:

```sh
uv sync --locked
uv run playwright install chromium
uv run shop login
```

On supported Linux distributions use `uv run playwright install --with-deps chromium`
to install OS dependencies too. The source works across macOS, Windows and Linux;
login needs a display. Unattended cloud authentication is not guaranteed. See
[Playwright supported environments](https://playwright.dev/python/docs/intro).

Sign in yourself in the **dedicated browser** and press Enter in the terminal.
The worker never copies your existing Chrome cookies or asks for your password.
Session expiry/CAPTCHA requires manual login; there is no bypass.

Private state defaults to `~/Library/Application Support/portfolio-shop` on macOS,
`%LOCALAPPDATA%\portfolio-shop` on Windows, and `$XDG_DATA_HOME/portfolio-shop`
(or `~/.local/share/portfolio-shop`) on Linux. Override with `shop --home PATH ...`.
Keep that directory outside Git and shared folders. Session cookies grant account
access. POSIX permissions are restricted; Windows relies on your directory ACLs.
No screenshots, raw HTML, browser traces or passwords are recorded. Cart journals
and printed reports still contain private product identities and quantities.

## Private order manifests

Follow [examples/example.json](examples/example.json), which contains fabricated
products. Set `channel` to `fresh` or `amazon`. `quantity` means the **absolute
target packages in that cart for this order**, never a monthly forecast, reserve
or physical stock count. Include brand, variant and size in `title_contains`.
Use `forbidden_title` to exclude variants. No searching or substitutions occur.
ASIN/title matching does not establish seller, halal status or delivered price;
review these in the merchant interface before paying.

Import previously reviewed catalog identities without inferring order quantities:

```sh
uv run shop import-catalog /private/path/products.json \
  --source-list 'Example list' --channel fresh \
  --output /private/path/order.json
```

Source JSON has `items` with `list_name`, `status=saved_product_candidate`,
`amazon_asin` and `product_title`. Unmatched entries are skipped. All quantities are
`null` until you edit them; remove unneeded rows. Existing files are never overwritten.
Merchant wishlists and monthly quantities are not synchronized automatically.

Pass a path, or store a manifest as `<private state>/lists/staples.json` for the alias:

```sh
uv run shop validate /private/path/order.json
uv run shop prepare staples --dry-run
uv run shop prepare staples
uv run shop review --channel fresh
```

`validate` works offline. `--dry-run` reads the cart/products without adding anything.
Preparation adds only missing units and preserves unrelated items. Excess quantities,
duplicate offers, unreadable controls, changed titles, subscription selections and
unrecognized carts stop the run. Fresh empty-cart layouts are not yet observed and
are deliberately unsupported. Each single-unit addition is journaled before clicking
and verified through complete cart readback. Only the selected channel is modified.

`review` prints the cart and keeps the visible browser open for manual checkout.
There is no budget enforcement, delivery-window selection, delivered-total quote,
payment confirmation or final purchase button in the worker. `shop diagnose
--channel fresh` reports limited UI structure for adapter maintenance without cookies,
input values or raw HTML. Diagnostic labels can still contain private information.

## Recovery and tests

One process may use a profile at a time. Don't edit the cart from another tab/device
during preparation; a local lock cannot prevent remote edits. Readback detects
differences but cannot provide an atomic merchant transaction.

If an add times out, its journal stays `pending`. The next run reconciles an exact
expected result. An unchanged/different cart requires manual review: wait for pending
requests to settle, run `shop acknowledge --channel fresh`, type `REVIEWED`, then
prepare again. Never delete the journal to force a retry. There is no automatic
retry of ambiguous mutations and no claim of merchant-side idempotency.

```sh
uv sync --locked
uv run playwright install chromium
uv run pytest -q
```

Tests use fabricated data and intercepted browser requests, never a signed-in profile.
No owner data belongs in this public package. Live cart changes and deployment need
separate authorization. No cloud resources or databases are provisioned here.
