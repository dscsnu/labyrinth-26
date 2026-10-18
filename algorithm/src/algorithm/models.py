"""Static game content as Pydantic models.

The game's static content — waypoints, shapes and patterns — is authored by the
algorithm package and exported as a single JSON document. These models are the
typed, validated shape of that document.

They mirror the constraints the Go backend applies when it loads content
(``internal/teamstate/content.go`` ``newContent`` and ``checkNesting``, and the
``waypoint`` / ``shape`` / ``pattern`` tables in migration ``0003``), so an
export that validates here is guaranteed to load there. The fixed limits live
in this module as named constants and must be kept in step with the backend:

* ``MAX_WAYPOINTS`` = ``maxWaypoints`` (64)
* ``MAX_SHAPES`` = ``maxShapes`` (64)
* ``MAX_PATTERNS`` = ``maxPatterns`` (2048)
* ``MAX_PATTERN_POINTS`` = ``config.toml`` ``game.max_pattern_points`` (6)

A waypoint's ``id`` is a 4-digit numeric string; a shape's ``id`` is an
alphabetical slug (``square``, ``triangle``, ...); a pattern's ``id`` is an
opaque integer key. A pattern belongs to one shape and is a set of waypoint
ids, so their order is irrelevant and is normalised to ascending.
"""

from __future__ import annotations

from collections.abc import Hashable, Iterable
from typing import Annotated, TypeVar

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    StringConstraints,
    field_validator,
    model_validator,
)

# --- Fixed limits. Keep in step with the backend (see the module docstring). ---

#: Most waypoints a document may hold (backend ``maxWaypoints``).
MAX_WAYPOINTS: int = 64
#: Most shapes a document may hold (backend ``maxShapes``).
MAX_SHAPES: int = 64
#: Most patterns a document may hold (backend ``maxPatterns``).
MAX_PATTERNS: int = 2048
#: Most waypoints in one pattern (``config.toml`` ``game.max_pattern_points``).
MAX_PATTERN_POINTS: int = 6

#: A waypoint id: exactly four decimal digits, leading zeros allowed.
WaypointID = Annotated[str, StringConstraints(pattern=r"^[0-9]{4}$")]

#: A shape id: a lowercase slug starting with a letter (``square``, ``triangle``).
ShapeID = Annotated[str, StringConstraints(pattern=r"^[a-z][a-z0-9_-]{0,31}$")]

#: A hashable id type, used by ``Content._check_unique_ids`` for either string
#: ids (waypoints, shapes) or integer ids (patterns).
_ID = TypeVar("_ID", bound=Hashable)


class Waypoint(BaseModel):
    """A physical location with a QR code.

    Attributes:
        id: The 4-digit id printed on the QR code, e.g. ``"1001"``.
        name: A human-readable label.
        latitude: Degrees north, in ``[-90, 90]``.
        longitude: Degrees east, in ``[-180, 180]``.
    """

    model_config = ConfigDict(frozen=True, extra="forbid")

    id: WaypointID
    name: str
    latitude: Annotated[float, Field(ge=-90.0, le=90.0)]
    longitude: Annotated[float, Field(ge=-180.0, le=180.0)]


class Shape(BaseModel):
    """A pattern type: what a pattern traces on the map, e.g. ``square``.

    Attributes:
        id: The slug the game uses to name the shape, e.g. ``"square"``.
        name: A human-readable label, e.g. ``"Square"``.
    """

    model_config = ConfigDict(frozen=True, extra="forbid")

    id: ShapeID
    name: str


class Pattern(BaseModel):
    """One concrete set of waypoints that forms a shape.

    Attributes:
        id: An opaque integer key, unique within the document. It is the
            backend's ``pattern.id``.
        shape_id: The ``Shape.id`` this pattern belongs to.
        waypoints: The waypoint ids forming the pattern, as an unordered set.
            Validated to 1..``MAX_PATTERN_POINTS`` distinct ids and normalised
            to ascending order.
    """

    model_config = ConfigDict(frozen=True, extra="forbid")

    id: Annotated[int, Field(ge=1)]
    shape_id: ShapeID
    waypoints: list[WaypointID]

    @field_validator("waypoints")
    @classmethod
    def _check_waypoints(cls, value: list[str]) -> list[str]:
        """Validate the waypoint list and normalise it to ascending order."""
        if not 1 <= len(value) <= MAX_PATTERN_POINTS:
            raise ValueError(
                f"pattern has {len(value)} waypoints; "
                f"must be 1..{MAX_PATTERN_POINTS}"
            )
        if len(set(value)) != len(value):
            raise ValueError("pattern waypoints must be distinct")
        return sorted(value)


class Content(BaseModel):
    """The whole static game content: waypoints, shapes and patterns.

    Attributes:
        waypoints: Every waypoint on the map.
        shapes: Every selectable shape (pattern type).
        patterns: Every pattern, each belonging to one shape.

    Validation (in ``_validate_content``) enforces the cross-entity rules the
    backend applies at load: the fixed count limits, unique ids, that every
    pattern's ``shape_id`` and waypoints resolve, that no two patterns share
    the same waypoint set, and that no pattern is nested inside a larger
    pattern of the same shape.
    """

    model_config = ConfigDict(frozen=True, extra="forbid")

    waypoints: list[Waypoint]
    shapes: list[Shape]
    patterns: list[Pattern]

    @model_validator(mode="after")
    def _validate_content(self) -> Content:
        """Enforce the cross-entity invariants, in backend order."""
        self._check_counts()
        waypoint_ids = self._check_unique_ids(
            (w.id for w in self.waypoints), "waypoint"
        )
        shape_ids = self._check_unique_ids(
            (s.id for s in self.shapes), "shape"
        )
        self._check_unique_ids((p.id for p in self.patterns), "pattern")
        self._check_references(waypoint_ids, shape_ids)
        self._check_duplicate_sets()
        self._check_nesting()
        return self

    def _check_counts(self) -> None:
        """Fail if any fixed limit is exceeded."""
        for count, limit, kind in (
            (len(self.waypoints), MAX_WAYPOINTS, "waypoints"),
            (len(self.shapes), MAX_SHAPES, "shapes"),
            (len(self.patterns), MAX_PATTERNS, "patterns"),
        ):
            if count > limit:
                raise ValueError(
                    f"{count} {kind}; at most {limit} fit"
                )

    @staticmethod
    def _check_unique_ids(ids: Iterable[_ID], kind: str) -> set[_ID]:
        """Return the set of ids, failing on a duplicate."""
        values = list(ids)
        if len(set(values)) != len(values):
            raise ValueError(f"duplicate {kind} id")
        return set(values)

    def _check_references(
        self, waypoint_ids: set[str], shape_ids: set[str]
    ) -> None:
        """Fail if a pattern references an unknown shape or waypoint."""
        for pattern in self.patterns:
            if pattern.shape_id not in shape_ids:
                raise ValueError(
                    f"pattern {pattern.id}: unknown shape {pattern.shape_id!r}"
                )
            for waypoint_id in pattern.waypoints:
                if waypoint_id not in waypoint_ids:
                    raise ValueError(
                        f"pattern {pattern.id}: unknown waypoint {waypoint_id!r}"
                    )

    def _check_duplicate_sets(self) -> None:
        """Fail if two patterns are the same set of waypoints."""
        seen: dict[frozenset[str], int] = {}
        for pattern in self.patterns:
            key = frozenset(pattern.waypoints)
            if key in seen:
                raise ValueError(
                    f"patterns {seen[key]} and {pattern.id} use the same waypoints"
                )
            seen[key] = pattern.id

    def _check_nesting(self) -> None:
        """Fail if a pattern is inside a larger pattern of the same shape."""
        by_shape: dict[str, list[Pattern]] = {}
        for pattern in self.patterns:
            by_shape.setdefault(pattern.shape_id, []).append(pattern)
        for shape_id, patterns in by_shape.items():
            for small in patterns:
                for big in patterns:
                    if small.id == big.id:
                        continue
                    if _is_strict_subset(small.waypoints, big.waypoints):
                        raise ValueError(
                            f"shape {shape_id!r}: pattern {small.id} is inside "
                            f"pattern {big.id}, so it would complete first"
                        )

    def to_json(self, indent: int = 2) -> str:
        """Serialise the content to a pretty JSON string.

        Args:
            indent: Number of spaces per indentation level.

        Returns:
            The JSON representation of the document.
        """
        return self.model_dump_json(indent=indent)


def _is_strict_subset(small: list[str], big: list[str]) -> bool:
    """Report whether ``small`` is a strict subset of ``big``."""
    small_set = set(small)
    big_set = set(big)
    return small_set < big_set
