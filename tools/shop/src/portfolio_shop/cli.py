from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .model import Blocked, Manifest
from .state import default_home, locked, write_json


def resolve_manifest(home: Path, name: str) -> Path:
    path = Path(name).expanduser()
    if path.is_file():
        return path
    # Named lists are local private manifests, not merchant list quantities.
    if not name or any(c not in "abcdefghijklmnopqrstuvwxyz0123456789-_" for c in name):
        raise ValueError("Use a manifest file or a lowercase list alias")
    return home / "lists" / f"{name}.json"


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="Prepare a cart; checkout remains manual.")
    p.add_argument("--home", type=Path, default=default_home(), help="Private state/profile directory")
    p.add_argument("--headless", action="store_true", help="For an already authenticated profile")
    sub = p.add_subparsers(dest="command", required=True)
    sub.add_parser("login", help="Open dedicated browser; sign in manually")
    imp = sub.add_parser("import-catalog", help="Import identities; leave order quantities unset")
    imp.add_argument("source", type=Path)
    imp.add_argument("--source-list", required=True)
    imp.add_argument("--output", type=Path, required=True)
    imp.add_argument("--channel", choices=["amazon", "fresh"], required=True)
    for name in ("prepare", "validate"):
        cmd = sub.add_parser(name)
        cmd.add_argument("list", help="Private manifest path or alias")
        if name == "prepare":
            cmd.add_argument("--dry-run", action="store_true", help="Inspect without adding anything")
    for name in ("review", "diagnose", "acknowledge"):
        cmd = sub.add_parser(name)
        cmd.add_argument("--channel", choices=["amazon", "fresh"], default="amazon")
    return p


def main() -> int:
    args = parser().parse_args()
    try:
        home = args.home.expanduser().resolve()
        if args.command == "import-catalog":
            from .catalog import import_catalog
            count = import_catalog(args.source.expanduser(), args.source_list, args.output.expanduser(), args.channel)
            print(f"Imported {count} identities. Set order quantities before prepare; nothing ordered.")
            return 0
        manifest = None
        if args.command in {"prepare", "validate"}:
            manifest = Manifest.read(resolve_manifest(home, args.list))
        if args.command == "validate":
            print(json.dumps({"status": "valid", "channel": manifest.channel, "items": len(manifest.items)}))
            return 0
        if args.headless and args.command in {"login", "acknowledge"}:
            raise ValueError(f"{args.command} requires a visible browser")
        from .browser import Amazon, CART_URL, FRESH_CART_URL, ORIGIN, browser, diagnose
        from .runner import prepare

        with locked(home), browser(home, args.headless) as page:
            if args.command == "login":
                if args.headless:
                    raise ValueError("login requires a visible browser")
                page.goto(ORIGIN, wait_until="domcontentloaded")
                input("Sign in in the dedicated browser. Press Enter here when finished: ")
                print("Session kept in your private browser profile.")
            elif args.command == "prepare":
                result = prepare(Amazon(page, manifest.channel), manifest, home / "journal.json", args.dry_run)
                print(json.dumps(result, indent=2))
            elif args.command in {"review", "diagnose"}:
                if args.command == "diagnose":
                    page.goto(FRESH_CART_URL if args.channel == "fresh" else CART_URL, wait_until="domcontentloaded")
                    print(json.dumps(diagnose(page), indent=2))
                else:
                    from dataclasses import asdict
                    print(json.dumps({"cart": [asdict(r) for r in Amazon(page, args.channel).cart()]}, indent=2))
                if not args.headless:
                    input("Review or finish checkout yourself in the browser. Press Enter to close: ")
            elif args.command == "acknowledge":
                journal_path = home / "journal.json"
                if journal_path.exists():
                    pending = json.loads(journal_path.read_text(encoding="utf-8"))
                    if pending.get("status") == "pending" and pending.get("channel") != args.channel:
                        raise Blocked("Select the interrupted run's cart channel for acknowledgement")
                Amazon(page, args.channel).cart()
                if args.headless:
                    raise ValueError("Acknowledgement requires a visible manual cart review")
                answer = input("Check the cart and wait for pending requests. Type REVIEWED to clear the interrupted run: ")
                if answer != "REVIEWED":
                    raise Blocked("Interrupted run remains unresolved")
                write_json(home / "journal.json", {"status": "manually_reviewed"})
        return 0
    except KeyboardInterrupt:
        print("Stopped. Any pending add still requires cart review.", file=sys.stderr)
        return 130
    except (Blocked, ValueError, OSError, KeyError, TypeError, EOFError) as exc:
        print(f"Stopped: {exc}", file=sys.stderr)
        return 2
    except Exception as exc:
        # Playwright errors can contain page content/URLs. Keep output bounded and
        # avoid dumping account details, browser traces or session state.
        print(f"Stopped: {type(exc).__name__}. Run shop review; no automatic retry.", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
