"""Import product identities without converting reserves into orders."""
import json
import re
from pathlib import Path

from .state import write_json


def import_catalog(source: Path, list_name: str, destination: Path, channel: str) -> int:
    if destination.exists():
        raise ValueError("Destination exists; refusing to overwrite per-order quantities")
    raw = json.loads(source.read_text(encoding="utf-8"))
    items = []
    for row in raw["items"]:
        if row.get("list_name") != list_name or row.get("status") != "saved_product_candidate":
            continue
        asin, title = row.get("amazon_asin", ""), row.get("product_title", "")
        if not re.fullmatch(r"[A-Z0-9]{10}", asin) or not title.strip():
            raise ValueError("Invalid saved product identity")
        items.append({"asin": asin, "quantity": None, "title_contains": [title]})
    if not items:
        raise ValueError("No saved products found for that exact list name")
    if len({row["asin"] for row in items}) != len(items):
        raise ValueError("Duplicate product identities; review the source")
    write_json(destination, {"version": 1, "channel": channel, "items": items})
    return len(items)
