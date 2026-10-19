-- Placeholder bosses for local development: the 2025 theme's seven evil exes.
-- HP rises with each one; a cast deals 60–131 in this seed (pattern.damage,
-- multiplier 1), so the first boss takes two or three casts and the last
-- eight to seventeen. Idempotent.
INSERT INTO boss (serial, name, asset, description, hp) VALUES
    (1, 'Matthew Patel',     'bosses/1.png', 'The first evil ex. Summons demon hipster chicks and fights mid-song.', 200),
    (2, 'Lucas Lee',         'bosses/2.png', 'Skateboarding movie star with a stunt crew that does his fighting.', 300),
    (3, 'Todd Ingram',       'bosses/3.png', 'Vegan bassist with psychic powers. Allergic to dairy-based tactics.', 400),
    (4, 'Roxy Richter',      'bosses/4.png', 'Half-ninja who teleports through walls and hates being called a phase.', 500),
    (5, 'Kyle Katayanagi',   'bosses/5.png', 'One half of the twin keyboardists. Fights with sound and a summoned beast.', 600),
    (6, 'Ken Katayanagi',    'bosses/6.png', 'The other twin. Together they nearly won the battle of the bands.', 700),
    (7, 'Gideon Graves',     'bosses/7.png', 'The final evil ex, record mogul and founder of the league. Same for every team.', 1000)
ON CONFLICT (serial) DO UPDATE SET
    name = EXCLUDED.name, asset = EXCLUDED.asset, description = EXCLUDED.description, hp = EXCLUDED.hp;
