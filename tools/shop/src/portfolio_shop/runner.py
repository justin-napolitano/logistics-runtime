from __future__ import annotations

import json
from dataclasses import asdict
from pathlib import Path
from typing import Protocol

from .model import Blocked, CartRow, Item, Manifest, plan, quantities
from .state import write_json


class Merchant(Protocol):
    def cart(self) -> list[CartRow]: ...
    def inspect(self, item: Item) -> None: ...
    def add(self, item: Item) -> None: ...


def reconcile_pending(merchant: Merchant, journal: Path, channel: str) -> None:
    if not journal.exists():
        return
    previous = json.loads(journal.read_text(encoding="utf-8"))
    if previous.get("status") != "pending":
        return
    if previous.get("channel") != channel:
        raise Blocked("Interrupted run belongs to a different cart channel; review that channel first")
    current = quantities(merchant.cart())
    before, expected = previous["before"], previous["expected"]
    if current == expected:
        write_json(journal, {**previous, "status": "reconciled", "cart": current})
    elif current == before:
        # Could still be an in-flight request. Never automatically retry.
        raise Blocked("Previous add has no confirmed result. Review the cart, then use shop acknowledge.")
    else:
        raise Blocked("Cart differs from the interrupted run. Review it, then use shop acknowledge.")


def prepare(merchant: Merchant, manifest: Manifest, journal: Path, dry_run: bool = False) -> dict:
    reconcile_pending(merchant, journal, manifest.channel)
    baseline = merchant.cart()
    changes = plan(manifest, baseline)
    # Check every pending product BEFORE changing the cart.
    for item, _ in changes:
        merchant.inspect(item)
    if dry_run:
        return {"status": "preview", "additions": [
            {"asin": item.asin, "quantity": delta} for item, delta in changes
        ], "cart": [asdict(row) for row in baseline]}
    before = quantities(baseline)
    # Add ONE unit at a time: product quantity selection varies across Amazon layouts.
    for item, delta in changes:
        for _ in range(delta):
            merchant.inspect(item)
            if quantities(merchant.cart()) != before:
                raise Blocked("Cart changed during preparation; run review before continuing")
            # Cart navigation changed the current page, so return to the verified product.
            merchant.inspect(item)
            expected = dict(before)
            expected[item.asin] = expected.get(item.asin, 0) + 1
            write_json(journal, {
                "status": "pending", "asin": item.asin,
                "channel": manifest.channel,
                "before": before, "expected": expected,
            })
            # An exception intentionally leaves the journal pending.
            merchant.add(item)
            rows = merchant.cart()
            actual = quantities(rows)
            if actual != expected:
                raise Blocked("Add result is ambiguous; stop and review the cart")
            for row in rows:
                if row.asin == item.asin:
                    item.check_title(row.title)
            write_json(journal, {"status": "verified", "cart": actual})
            before = actual
    rows = merchant.cart()
    if quantities(rows) != before or plan(manifest, rows):
        raise Blocked("Final cart changed; review manually")
    result = {"status": "ready_for_manual_checkout", "cart": [asdict(row) for row in rows]}
    write_json(journal, result)
    return result
