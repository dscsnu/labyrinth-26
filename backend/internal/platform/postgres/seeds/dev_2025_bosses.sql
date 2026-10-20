-- Placeholder bosses for local development: the 2025 theme's evil exes, the
-- Katayanagi twins fought as one boss. HP: 25, 100, 250, 510, 900, 1500
-- (3,285 in all). A cast deals 60–131 in this seed (pattern.damage,
-- multiplier 1), so boss 1 falls to one cast and Gideon takes 12 to 25.
-- Each boss attacks every attack_interval_s seconds of its phase for
-- attack_damage health (game.health is 10): faster and harder later on, so a
-- team that stalls on Gideon is knocked out in 10 minutes. Idempotent; it
-- removes bosses past the sixth, so an older seed's boss 7 doesn't stop the
-- backend starting (it fails if a cast already hit one: reseed a fresh
-- database).
DELETE FROM boss WHERE serial > 6;
INSERT INTO boss (serial, name, asset, description, hp, attack_interval_s, attack_damage) VALUES
    (1, 'Matthew Patel', 'bosses/1.png', 'The first evil ex. Summons demon hipster chicks and fights mid-song.', 25, 300, 1),
    (2, 'Lucas Lee', 'bosses/2.png', 'Skateboarding movie star with a stunt crew that does his fighting.', 100, 300, 1),
    (3, 'Todd Ingram', 'bosses/3.png', 'Vegan bassist with psychic powers. Allergic to dairy-based tactics.', 250, 270, 1),
    (4, 'Roxy Richter', 'bosses/4.png', 'Half-ninja who teleports through walls and hates being called a phase.', 510, 240, 1),
    (5, 'Ken & Kyle Katayanagi', 'bosses/5.png', 'The twin keyboardists, fought together. Sound waves and a summoned beast.', 900, 240, 1),
    (6, 'Gideon Graves', 'bosses/6.png', 'The final evil ex, record mogul and founder of the league. Same for every team.', 1500, 120, 2)
ON CONFLICT (serial) DO UPDATE SET
    name = EXCLUDED.name, asset = EXCLUDED.asset, description = EXCLUDED.description, hp = EXCLUDED.hp,
    attack_interval_s = EXCLUDED.attack_interval_s, attack_damage = EXCLUDED.attack_damage;
