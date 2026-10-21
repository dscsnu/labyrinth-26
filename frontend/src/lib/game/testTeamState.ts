// TEST DATA, not live: a team_state as the backend sends it (the message of
// an /api/events "team_state" event, and GET /api/teams/me/state), so the
// top bar and the map can be built before the QR scanners are. /play shows
// it in place of its live state; see ForRohitJG.MD for what reads it and how
// to replace it with the real thing.
//
// The numbers are the 2026 seed's and config.toml's, a fight in progress.
// Times are relative to the page load, so the attack countdown always runs:
//   -18 min  the team starts fighting boss 1, Matthew Patel (25 HP)
//    -8 min  he attacks for 1: health 10 -> 9
//    -7 min  the triangle 2001-2004-2008 (pattern 64, 70 damage) defeats
//            him; the fight with boss 2, Lucas Lee (100 HP), begins
//    -2 min  the line 2001-2003 (pattern 2, 31 damage) hits Lucas: 69/100 HP.
//            Its points go on QR cooldown, and line (every line is class_2)
//            on class cooldown until the team casts another class
//  -1.5 min  the leader selects the quadrilateral
//  -1, -0.5  2002 (Arcade) and 2006 (AIC) are scanned: those markers wiggle
//    +3 min  the line's points come off QR cooldown (game.qr_cooldown, 5 min)
//    +3 min  Lucas attacks (game.boss_attack.interval, 10 min into the fight)
// User IDs are placeholders.
import type { Cast, TeamState } from "#lib/types/Game.type.ts";

const LOADED = Date.now();
const at = (minutes: number) => new Date(LOADED + minutes * 60_000).toISOString();

const LEADER = "6f1c2b9e-4d3a-4f6b-9a1e-2c7d8e9f0a1b";
const MEMBER = "a3e5d7c9-1b2f-4e6a-8c0d-9f8e7d6c5b4a";

// The team's two casts: the line is its last_cast.
const LINE_CAST: Cast = {
    pattern: ["2001", "2003"],
    shape: "line",
    class: "class_2",
    cast_by: LEADER,
    cast_at: at(-2),
    already_completed: false,
    damage: 31,
    dealt: 31,
    boss: 2,
    defeated_boss: false,
    instant: false,
    boosted: false,
    short_qr_cooldown: false,
};
const TRIANGLE_CAST: Cast = {
    pattern: ["2001", "2004", "2008"],
    shape: "triangle",
    class: "class_3",
    cast_by: MEMBER,
    cast_at: at(-7),
    already_completed: false,
    damage: 70,
    dealt: 70,
    boss: 1,
    defeated_boss: true,
    instant: false,
    boosted: false,
    short_qr_cooldown: false,
};

// The cast log, as GET /api/teams/me/casts returns it: newest first. The
// spell book's "Discovered" lists read it.
export const testCasts: Cast[] = [LINE_CAST, TRIANGLE_CAST];

export const testTeamState: TeamState = {
    team_id: "ABC123",
    waypoints: ["2001", "2002", "2003", "2004", "2006", "2008"],
    patterns: [
        ["2001", "2003"],
        ["2001", "2004", "2008"],
    ],
    shapes: ["line", "triangle"],
    selection: {
        shape: "quadrilateral",
        selected_by: LEADER,
        selected_at: at(-1.5),
    },
    last_cast: LINE_CAST,
    // The current attempt, oldest first: these waypoints wiggle on the map.
    stack: [
        { waypoint_id: "2002", user_id: MEMBER, scanned_at: at(-1), wrong: false },
        { waypoint_id: "2006", user_id: LEADER, scanned_at: at(-0.5), wrong: false },
    ],
    poisoned: false,
    bosses_defeated: 1,
    boss: {
        serial: 2,
        damage: 31,
        phase_started_at: at(-7),
        attacks: 0,
        next_attack_at: at(3),
    },
    damage_multiplier: 1,
    health: 9,
    knocked_out: false,
    unlocked_points: 4,
    locked_shapes: ["pentagon", "hexagon"],
    cooldown_class: "class_2",
    cooldown_shapes: ["line"],
    qr_cooldowns: [
        { waypoint_id: "2001", until: at(3) },
        { waypoint_id: "2003", until: at(3) },
    ],
    cast_count: 2,
    powerups: { held: [], active: [] },
    qte: null,
    version: { epoch: 1792620000000, seq: 9 },
};
