from __future__ import annotations

import json
import os
import sys
from contextlib import contextmanager
from pathlib import Path

from .model import Blocked


def default_home() -> Path:
    if sys.platform == "win32":
        root = Path(os.environ.get("LOCALAPPDATA", Path.home() / "AppData/Local"))
    elif sys.platform == "darwin":
        root = Path.home() / "Library/Application Support"
    else:
        root = Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local/share"))
    return root / "portfolio-shop"


def private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if os.name != "nt":
        path.chmod(0o700)


def write_json(path: Path, value: dict) -> None:
    """Atomic, private durable journal, written before a browser mutation."""
    private_dir(path.parent)
    temp = path.with_suffix(".tmp")
    with temp.open("w", encoding="utf-8") as handle:
        if os.name != "nt":
            os.chmod(temp, 0o600)
        json.dump(value, handle, indent=2)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temp, path)


@contextmanager
def locked(home: Path):
    private_dir(home)
    # OS releases this advisory lock after crashes. The file is intentionally kept.
    with (home / "worker.lock").open("a+b") as handle:
        if os.name != "nt":
            os.chmod(home / "worker.lock", 0o600)
        try:
            if os.name == "nt":
                import msvcrt

                handle.seek(0)
                handle.write(b"0")
                handle.flush()
                handle.seek(0)
                msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl

                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            raise Blocked("Another shop process is using this profile") from exc
        try:
            yield
        finally:
            if os.name == "nt":
                handle.seek(0)
                msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(handle, fcntl.LOCK_UN)
