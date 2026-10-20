-- Placeholder bosses for local development: the 2025 theme's evil exes, the
-- Katayanagi twins fought as one boss. HP: 25, 100, 250, 510, 900, 1500
-- (3,285 in all). A cast deals 60–131 in this seed (pattern.damage,
-- multiplier 1), so boss 1 falls to one cast and Gideon takes 12 to 25.
-- How bosses attack is in config.toml ([game.boss_attack]), not here.
-- Idempotent; it
-- removes bosses past the sixth, so an older seed's boss 7 doesn't stop the
-- backend starting (it fails if a cast already hit one: reseed a fresh
-- database).
DELETE FROM boss WHERE serial > 6;
INSERT INTO boss (serial, name, asset, description, hp) VALUES
    (1, 'Matthew Patel', 'bosses/1.png', 'The first evil ex. Summons demon hipster chicks and fights mid-song.', 25),
    (2, 'Lucas Lee', 'bosses/2.png', 'Skateboarding movie star with a stunt crew that does his fighting.', 100),
    (3, 'Todd Ingram', 'bosses/3.png', 'Vegan bassist with psychic powers. Allergic to dairy-based tactics.', 250),
    (4, 'Roxy Richter', 'bosses/4.png', 'Half-ninja who teleports through walls and hates being called a phase.', 510),
    (5, 'Ken & Kyle Katayanagi', 'bosses/5.png', 'The twin keyboardists, fought together. Sound waves and a summoned beast.', 900),
    (6, 'Gideon Graves', 'bosses/6.png', 'The final evil ex, record mogul and founder of the league. Same for every team.', 1500)
ON CONFLICT (serial) DO UPDATE SET
    name = EXCLUDED.name, asset = EXCLUDED.asset, description = EXCLUDED.description, hp = EXCLUDED.hp;
