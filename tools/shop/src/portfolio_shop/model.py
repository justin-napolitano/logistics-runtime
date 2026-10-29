from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path


class Blocked(RuntimeError):
    """A run requires manual review; do not retry a mutation blindly."""


@dataclass(frozen=True)
class Item:
    asin: str
    quantity: int
    title_contains: tuple[str, ...]
    forbidden_title: tuple[str, ...] = ()

    def check_title(self, title: str) -> None:
        normalized = " ".join(title.casefold().split())
        if any(s.casefold() not in normalized for s in self.title_contains):
            raise Blocked(f"{self.asin}: product title/package does not match manifest")
        if any(s.casefold() in normalized for s in self.forbidden_title):
            raise Blocked(f"{self.asin}: forbidden product variant")


@dataclass(frozen=True)
class Manifest:
    channel: str
    items: tuple[Item, ...]

    @classmethod
    def read(cls, path: Path) -> Manifest:
        raw = json.loads(path.read_text(encoding="utf-8"))
        if set(raw) != {"version", "channel", "items"} or raw["version"] != 1:
            raise ValueError("Expected version=1, channel and items only")
        if raw["channel"] not in {"amazon", "fresh"}:
            raise ValueError("channel must be amazon or fresh")
        if not isinstance(raw["items"], list) or not 1 <= len(raw["items"]) <= 100:
            raise ValueError("Expected 1–100 items")
        items = []
        for row in raw["items"]:
            if not isinstance(row, dict) or set(row) - {
                "asin", "quantity", "title_contains", "forbidden_title"
            }:
                raise ValueError("Unknown item field")
            asin = row.get("asin", "")
            if not isinstance(asin, str) or not re.fullmatch(r"[A-Z0-9]{10}", asin):
                raise ValueError("Each item requires a ten-character ASIN")
            quantity = row.get("quantity")
            if type(quantity) is not int or not 1 <= quantity <= 50:
                raise ValueError(f"{asin}: set an explicit per-order quantity from 1 to 50")
            includes = row.get("title_contains")
            excludes = row.get("forbidden_title", [])
            for values in (includes, excludes):
                if not isinstance(values, list) or any(
                    not isinstance(s, str) or not s.strip() or len(s) > 200 for s in values
                ):
                    raise ValueError("Title rules must be lists of nonempty strings")
            if not includes:
                raise ValueError(f"{asin}: include brand, variant and package in title_contains")
            items.append(Item(asin, quantity, tuple(includes), tuple(excludes)))
        if len({i.asin for i in items}) != len(items):
            raise ValueError("Duplicate ASINs are not allowed")
        return cls(raw["channel"], tuple(items))


@dataclass(frozen=True)
class CartRow:
    asin: str
    title: str
    quantity: int


def quantities(rows: list[CartRow]) -> dict[str, int]:
    result: dict[str, int] = {}
    for row in rows:
        if row.asin in result:
            raise Blocked(f"{row.asin}: multiple offers/rows; review manually")
        if row.quantity < 1:
            raise Blocked("Cart contains an unreadable quantity")
        result[row.asin] = row.quantity
    return result


def plan(manifest: Manifest, rows: list[CartRow]) -> list[tuple[Item, int]]:
    current = quantities(rows)
    wanted = {i.asin: i for i in manifest.items}
    for row in rows:
        if row.asin in wanted:
            wanted[row.asin].check_title(row.title)
    changes = []
    for item in manifest.items:
        have = current.get(item.asin, 0)
        if have > item.quantity:
            raise Blocked(f"{item.asin}: cart exceeds target; no automatic removal")
        if item.quantity > have:
            changes.append((item, item.quantity - have))
    return changes
