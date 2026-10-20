-- Placeholder bosses for local development: the 2025 theme's seven evil exes.
-- HP rises with each one; a cast deals 60–131 in this seed (pattern.damage,
-- multiplier 1), so the first boss takes two to four casts and the last
-- eight to seventeen. Idempotent.
-- Each boss attacks every attack_interval_s seconds of its phase for
-- attack_damage health (game.health is 10): faster and harder later on, so a
-- team that stalls on boss 7 is knocked out in 10 minutes.
INSERT INTO boss (serial, name, asset, description, hp, attack_interval_s, attack_damage) VALUES
    (1, 'Matthew Patel', 'bosses/1.png', 'The first evil ex. Summons demon hipster chicks and fights mid-song.', 200, 300, 1),
    (2, 'Lucas Lee', 'bosses/2.png', 'Skateboarding movie star with a stunt crew that does his fighting.', 300, 300, 1),
    (3, 'Todd Ingram', 'bosses/3.png', 'Vegan bassist with psychic powers. Allergic to dairy-based tactics.', 400, 270, 1),
    (4, 'Roxy Richter', 'bosses/4.png', 'Half-ninja who teleports through walls and hates being called a phase.', 500, 240, 1),
    (5, 'Kyle Katayanagi', 'bosses/5.png', 'One half of the twin keyboardists. Fights with sound and a summoned beast.', 600, 240, 1),
    (6, 'Ken Katayanagi', 'bosses/6.png', 'The other twin. Together they nearly won the battle of the bands.', 700, 180, 2),
    (7, 'Gideon Graves', 'bosses/7.png', 'The final evil ex, record mogul and founder of the league. Same for every team.', 1000, 120, 2)
ON CONFLICT (serial) DO UPDATE SET
    name = EXCLUDED.name, asset = EXCLUDED.asset, description = EXCLUDED.description, hp = EXCLUDED.hp,
    attack_interval_s = EXCLUDED.attack_interval_s, attack_damage = EXCLUDED.attack_damage;
