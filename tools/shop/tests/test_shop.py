import json
from dataclasses import replace

import pytest

from portfolio_shop.model import Blocked, CartRow, Item, Manifest, plan
from portfolio_shop.runner import prepare
from portfolio_shop.state import locked, write_json
from portfolio_shop.catalog import import_catalog


ITEM = Item("B000TEST01", 2, ("Example", "8 oz"), ("Mini",))
MANIFEST = Manifest("amazon", (ITEM,))
OTHER = CartRow("B000OTHER1", "Unrelated item", 3)


class Fake:
    def __init__(self, rows=(), fault=None):
        self.rows = list(rows)
        self.calls = 0
        self.fault = fault

    def cart(self):
        return self.rows[:]

    def inspect(self, item):
        item.check_title("Example plain 8 oz")

    def add(self, item):
        self.calls += 1
        if self.fault == "before":
            raise TimeoutError("unknown result")
        match = next((r for r in self.rows if r.asin == item.asin), None)
        if match:
            self.rows[self.rows.index(match)] = replace(match, quantity=match.quantity + 1)
        else:
            self.rows.append(CartRow(item.asin, "Example plain 8 oz", 1))
        if self.fault == "after":
            raise TimeoutError("response lost after add")


def test_absolute_targets_preserve_unrelated_and_rerun(tmp_path):
    fake = Fake([OTHER, CartRow(ITEM.asin, "Example plain 8 oz", 1)])
    journal = tmp_path / "journal.json"
    assert prepare(fake, MANIFEST, journal)["status"] == "ready_for_manual_checkout"
    assert fake.calls == 1
    assert OTHER in fake.rows
    prepare(fake, MANIFEST, journal)
    assert fake.calls == 1


def test_crash_after_committed_add_reconciles_without_duplicate(tmp_path):
    fake = Fake([OTHER], "after")
    manifest = Manifest("amazon", (replace(ITEM, quantity=1),))
    journal = tmp_path / "journal.json"
    with pytest.raises(TimeoutError):
        prepare(fake, manifest, journal)
    assert json.loads(journal.read_text())["status"] == "pending"
    fake.fault = None
    prepare(fake, manifest, journal)
    assert fake.calls == 1


def test_unknown_result_not_retried_even_when_cart_unchanged(tmp_path):
    fake = Fake([OTHER], "before")
    journal = tmp_path / "journal.json"
    with pytest.raises(TimeoutError):
        prepare(fake, MANIFEST, journal)
    fake.fault = None
    with pytest.raises(Blocked, match="acknowledge"):
        prepare(fake, MANIFEST, journal)
    assert fake.calls == 1


def test_changed_other_item_blocks_pending_recovery(tmp_path):
    fake = Fake([OTHER], "after")
    journal = tmp_path / "journal.json"
    with pytest.raises(TimeoutError):
        prepare(fake, MANIFEST, journal)
    fake.rows[0] = replace(OTHER, quantity=4)
    with pytest.raises(Blocked, match="differs"):
        prepare(fake, MANIFEST, journal)


def test_dry_run_never_adds(tmp_path):
    fake = Fake([OTHER])
    assert prepare(fake, MANIFEST, tmp_path / "journal.json", True)["additions"] == [
        {"asin": ITEM.asin, "quantity": 2}
    ]
    assert fake.calls == 0


def test_all_products_checked_before_first_add(tmp_path):
    fake = Fake([OTHER])
    original = fake.inspect

    def inspect(item):
        if item.asin == "B000TEST02":
            raise Blocked("Unavailable")
        original(item)

    fake.inspect = inspect
    manifest = Manifest("amazon", (ITEM, replace(ITEM, asin="B000TEST02")))
    with pytest.raises(Blocked, match="Unavailable"):
        prepare(fake, manifest, tmp_path / "journal.json")
    assert fake.calls == 0


@pytest.mark.parametrize("rows", [
    [CartRow(ITEM.asin, "Example Mini 8 oz", 1)],
    [CartRow(ITEM.asin, "Example plain 16 oz", 1)],
    [CartRow(ITEM.asin, "Example plain 8 oz", 3)],
    [OTHER, OTHER],
])
def test_mismatch_excess_and_duplicate_offers_block(rows):
    with pytest.raises(Blocked):
        plan(MANIFEST, rows)


@pytest.mark.parametrize("quantity", [None, 0, -1, 51, True, "2"])
def test_per_order_quantity_must_be_explicit(tmp_path, quantity):
    path = tmp_path / "list.json"
    write_json(path, {"version": 1, "channel": "fresh", "items": [{
        "asin": ITEM.asin, "quantity": quantity, "title_contains": ["Example", "8 oz"],
    }]})
    with pytest.raises(ValueError, match="per-order"):
        Manifest.read(path)


def test_profile_single_writer_lock(tmp_path):
    with locked(tmp_path):
        with pytest.raises(Blocked, match="Another"):
            with locked(tmp_path):
                pytest.fail("Acquired another writer")


def test_pending_run_is_bound_to_cart_channel(tmp_path):
    fake = Fake([OTHER], "after")
    journal = tmp_path / "journal.json"
    with pytest.raises(TimeoutError):
        prepare(fake, MANIFEST, journal)
    with pytest.raises(Blocked, match="different cart channel"):
        prepare(fake, Manifest("fresh", MANIFEST.items), journal)
    assert fake.calls == 1


def test_import_does_not_turn_stock_targets_into_order(tmp_path):
    source, output = tmp_path / "source.json", tmp_path / "order.json"
    write_json(source, {"items": [
        {"list_name": "Demo", "status": "saved_product_candidate", "amazon_asin": ITEM.asin,
         "product_title": "Example plain 8 oz", "quantity_reference": "200 cases per year"},
        {"list_name": "Demo", "status": "unmatched"},
        {"list_name": "Other", "status": "saved_product_candidate"},
    ]})
    assert import_catalog(source, "Demo", output, "fresh") == 1
    result = json.loads(output.read_text())
    assert result["items"][0]["quantity"] is None
    with pytest.raises(ValueError, match="per-order"):
        Manifest.read(output)
    with pytest.raises(ValueError, match="exists"):
        import_catalog(source, "Demo", output, "fresh")
