# algorithm

Authors and exports the game's **static content** — waypoints, shapes and
patterns — as a single JSON document, validated with [Pydantic][pydantic].

The JSON is the source of truth the backend consumes: the models mirror the
constraints the Go backend applies when it loads content
(`backend/internal/teamstate/content.go`, `newContent` and `checkNesting`, and
the `waypoint` / `shape` / `pattern` tables in migration `0003`), so a document
that validates here is guaranteed to load there.

## Install

```bash
uv sync            # creates .venv, installs the package + dev deps
uv run algorithm   # print the bundled 2025 sample as JSON
uv run pytest      # run the tests
```

Python ≥ 3.13, Pydantic ≥ 2.14.

## CLI

```bash
uv run algorithm                     # validate + pretty-print the sample
uv run algorithm --out content.json  # write to a file instead
uv run algorithm --source other.json # validate/export an arbitrary document
```

`--source` defaults to `data/2025.json` (the 2025 seed, converted to JSON). The
CLI reads a document, validates it, and re-emits pretty JSON — use it as a
round-trip check.

## Schema

One JSON object with three arrays:

```json
{
  "waypoints": [
    {"id": "1001", "name": "Tower 6 FruitShop (T6)", "latitude": 28.52836, "longitude": 77.57777}
  ],
  "shapes": [
    {"id": "square", "name": "Square"}
  ],
  "patterns": [
    {"id": 87, "shape_id": "square", "waypoints": ["1001", "1002", "1007", "1008"]}
  ]
}
```

### `Waypoint`

| Field | Type | Rule |
|---|---|---|
| `id` | `str` | exactly 4 decimal digits (`^[0-9]{4}$`) |
| `name` | `str` | any |
| `latitude` | `float` | `[-90, 90]` |
| `longitude` | `float` | `[-180, 180]` |

### `Shape`

| Field | Type | Rule |
|---|---|---|
| `id` | `str` | lowercase slug starting with a letter (`^[a-z][a-z0-9_-]{0,31}$`), e.g. `square` |
| `name` | `str` | any |

### `Pattern`

| Field | Type | Rule |
|---|---|---|
| `id` | `int` | opaque positive integer, unique (the backend's `pattern.id`) |
| `shape_id` | `str` | must be a known `Shape.id` |
| `waypoints` | `list[str]` | 1..6 distinct waypoint ids; order is irrelevant and normalised to ascending |

A pattern is a **set** of waypoints: `["1001", "1002"]` and `["1002", "1001"]`
are the same pattern.

## Cross-entity validation

`Content` enforces, in this order (matching the backend's `newContent`):

1. count limits: ≤ 64 waypoints, ≤ 64 shapes, ≤ 2048 patterns
2. unique waypoint ids, shape ids, pattern ids
3. every `shape_id` and every waypoint resolves
4. no two patterns share the same waypoint set
5. no pattern is nested inside a larger pattern of the same shape

## Limits and where they live

| Limit | Here | Backend |
|---|---|---|
| 64 waypoints | `MAX_WAYPOINTS` | `maxWaypoints` (`content.go`) |
| 64 shapes | `MAX_SHAPES` | `maxShapes` (`content.go`) |
| 2048 patterns | `MAX_PATTERNS` | `maxPatterns` (`content.go`) |
| 6 points per pattern | `MAX_PATTERN_POINTS` | `game.max_pattern_points` (`config.toml`) |

Keep these in step when you change one side.

## Using the models in code

```python
from algorithm.models import Content, Pattern, Shape, Waypoint

content = Content(
    waypoints=[Waypoint(id="1001", name="Tower 6", latitude=28.52836, longitude=77.57777)],
    shapes=[Shape(id="square", name="Square")],
    patterns=[Pattern(id=1, shape_id="square", waypoints=["1001", "1002", "1007", "1008"])],
)

content.to_json()                                  # pretty JSON string
Content.model_validate_json(raw_json_string)        # parse + validate
```

Models are `frozen=True` (immutable) and `extra="forbid"` (unknown keys fail),
so a typo is a hard error.

[pydantic]: https://docs.pydantic.dev/
