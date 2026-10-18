"""Tests for the content models in :mod:`algorithm.models`.

Each rejection case mirrors a check the Go backend applies at load
(``internal/teamstate/content.go`` ``newContent`` and ``checkNesting``).
"""

from __future__ import annotations

from pathlib import Path

import pytest
from pydantic import ValidationError

from algorithm.models import Content, Pattern, Shape, Waypoint

#: The bundled 2025 sample, used as a known-valid document.
SAMPLE = Path(__file__).resolve().parents[1] / "data" / "2025.json"


def _content(
    *,
    waypoints: list[Waypoint] | None = None,
    shapes: list[Shape] | None = None,
    patterns: list[Pattern] | None = None,
) -> Content:
    """Build a minimal valid content document, overriding any part."""
    return Content(
        waypoints=waypoints or [
            Waypoint(id="1001", name="A", latitude=28.0, longitude=77.0),
            Waypoint(id="1002", name="B", latitude=28.1, longitude=77.1),
            Waypoint(id="1003", name="C", latitude=28.2, longitude=77.2),
        ],
        shapes=shapes or [Shape(id="square", name="Square")],
        patterns=patterns or [
            Pattern(id=1, shape_id="square", waypoints=["1001", "1002", "1003"])
        ],
    )


def test_sample_loads_and_round_trips() -> None:
    """The bundled 2025 seed validates and re-serialises to itself."""
    content = Content.model_validate_json(SAMPLE.read_text())
    assert len(content.waypoints) == 11
    assert len(content.shapes) == 5
    assert len(content.patterns) == 359
    assert Content.model_validate_json(content.to_json()) == content


def test_waypoint_id_must_be_four_digits() -> None:
    """Waypoint ids must be exactly four decimal digits."""
    for bad in ("A1", "10001", "10A1", "", " 1001"):
        with pytest.raises(ValidationError):
            _content(waypoints=[Waypoint(id=bad, name="x", latitude=0, longitude=0)])


def test_waypoint_coordinates_in_range() -> None:
    """Latitude and longitude must be in valid ranges."""
    with pytest.raises(ValidationError):
        _content(waypoints=[Waypoint(id="1001", name="x", latitude=91, longitude=0)])
    with pytest.raises(ValidationError):
        _content(waypoints=[Waypoint(id="1001", name="x", latitude=0, longitude=181)])


def test_shape_id_is_lowercase_slug() -> None:
    """Shape ids must start with a lowercase letter."""
    for bad in ("Square", "1234", "-square", "", "sq!"):
        with pytest.raises(ValidationError):
            _content(shapes=[Shape(id=bad, name="x")])


def test_pattern_waypoints_size_and_duplicates() -> None:
    """A pattern needs 1..MAX_PATTERN_POINTS distinct waypoints."""
    with pytest.raises(ValidationError):
        _content(patterns=[Pattern(id=1, shape_id="square", waypoints=[])])
    with pytest.raises(ValidationError):
        _content(patterns=[Pattern(id=1, shape_id="square", waypoints=["1001", "1001"])])
    with pytest.raises(ValidationError):
        _content(patterns=[Pattern(id=1, shape_id="square", waypoints=["1001"] * 7)])


def test_pattern_waypoints_are_normalised() -> None:
    """Waypoint order is irrelevant and is normalised to ascending."""
    content = _content(patterns=[Pattern(id=1, shape_id="square", waypoints=["1003", "1001", "1002"])])
    assert content.patterns[0].waypoints == ["1001", "1002", "1003"]


def test_unknown_shape_reference_rejected() -> None:
    """A pattern naming an unknown shape is rejected."""
    with pytest.raises(ValidationError, match="unknown shape"):
        _content(patterns=[Pattern(id=1, shape_id="triangle", waypoints=["1001"])])


def test_unknown_waypoint_reference_rejected() -> None:
    """A pattern naming an unknown waypoint is rejected."""
    with pytest.raises(ValidationError, match="unknown waypoint"):
        _content(patterns=[Pattern(id=1, shape_id="square", waypoints=["9999"])])


def test_duplicate_pattern_ids_rejected() -> None:
    """Two patterns may not share an id."""
    with pytest.raises(ValidationError, match="duplicate pattern id"):
        _content(patterns=[
            Pattern(id=1, shape_id="square", waypoints=["1001"]),
            Pattern(id=1, shape_id="square", waypoints=["1002"]),
        ])


def test_duplicate_waypoint_sets_rejected() -> None:
    """Two patterns may not be the same set of waypoints."""
    with pytest.raises(ValidationError, match="same waypoints"):
        _content(patterns=[
            Pattern(id=1, shape_id="square", waypoints=["1001", "1002"]),
            Pattern(id=2, shape_id="square", waypoints=["1002", "1001"]),
        ])


def test_nested_pattern_rejected() -> None:
    """A pattern inside a larger pattern of the same shape is rejected."""
    with pytest.raises(ValidationError, match="would complete first"):
        _content(patterns=[
            Pattern(id=1, shape_id="square", waypoints=["1001", "1002", "1003"]),
            Pattern(id=2, shape_id="square", waypoints=["1001", "1002"]),
        ])


def test_nesting_across_shapes_allowed() -> None:
    """The same set may nest in a different shape without complaint."""
    _content(
        shapes=[Shape(id="square", name="Square"), Shape(id="line", name="Line")],
        patterns=[
            Pattern(id=1, shape_id="square", waypoints=["1001", "1002", "1003"]),
            Pattern(id=2, shape_id="line", waypoints=["1001", "1002"]),
        ],
    )


def test_waypoint_count_limit() -> None:
    """At most MAX_WAYPOINTS waypoints are allowed."""
    waypoints = [
        Waypoint(id=f"{1000 + i:04d}", name="x", latitude=0, longitude=0)
        for i in range(64)
    ]
    _content(waypoints=waypoints)  # exactly the limit is fine
    with pytest.raises(ValidationError, match="waypoints"):
        _content(waypoints=waypoints + [Waypoint(id="1064", name="x", latitude=0, longitude=0)])
