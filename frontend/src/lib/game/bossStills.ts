// Each boss's still, static/bosses_animation/<serial>-still.png, by serial:
// its idle's first frame cut to BossSprite's crop (the box round every idle
// sprite), so it's exactly what the sprite shows first. Shown while the
// spritesheet loads. Regenerate both with the sheets.
export const bossStillUrl = (serial: number) => `/bosses_animation/${serial}-still.png`;

export const BOSS_STILLS: Record<number, { w: number; h: number }> = {
    1: { w: 192, h: 237 },
    2: { w: 165, h: 261 },
    3: { w: 187, h: 229 },
    4: { w: 195, h: 194 },
    5: { w: 169, h: 264 },
    6: { w: 179, h: 185 },
};
