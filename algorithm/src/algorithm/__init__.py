"""The algorithm package: authors and exports the game's static content.

The package validates and serialises waypoints, shapes and patterns as a
single JSON document (see :mod:`algorithm.models`). ``main`` is a small CLI
that loads a content document, validates it, and writes it out, so the
exported JSON can be inspected or piped onward.

Example::

    algorithm                  # print the bundled 2025 sample as JSON
    algorithm --out content.json
    algorithm --source other.json --out other.pretty.json
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from pydantic import ValidationError

from .models import Content

#: The package root (the directory containing ``src``).
_PACKAGE_ROOT: Path = Path(__file__).resolve().parents[2]

#: The bundled sample content document.
DEFAULT_SOURCE: Path = _PACKAGE_ROOT / "data" / "2025.json"


def main(argv: list[str] | None = None) -> int:
    """Load, validate and export a content document.

    Args:
        argv: Command-line arguments; defaults to ``sys.argv[1:]``.

    Returns:
        Exit code: ``0`` on success, ``1`` on a bad document or unreadable file.
    """
    parser = argparse.ArgumentParser(
        prog="algorithm",
        description="Validate and export game content as JSON.",
    )
    parser.add_argument(
        "--source",
        type=Path,
        default=DEFAULT_SOURCE,
        help="content JSON to read (default: the bundled 2025 sample)",
    )
    parser.add_argument(
        "--out",
        type=Path,
        help="write the pretty JSON to this file instead of stdout",
    )
    args = parser.parse_args(argv)

    try:
        content = Content.model_validate_json(args.source.read_text())
    except FileNotFoundError:
        print(f"algorithm: source not found: {args.source}", file=sys.stderr)
        return 1
    except ValidationError as exc:
        print(f"algorithm: invalid content: {args.source}", file=sys.stderr)
        print(exc, file=sys.stderr)
        return 1

    rendered = content.to_json()
    if args.out:
        args.out.write_text(rendered + "\n")
        print(f"wrote {args.out}")
    else:
        print(rendered)
    return 0
