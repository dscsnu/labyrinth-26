// Where a spell's vertices are drawn, on SpellFigure's 32×32 grid: a
// regular polygon of its fewest points, round the centre, pointing up when
// odd and sitting flat when even (a level line, a square, a flat-topped
// hexagon). A concave quadrilateral (variant "Concave") is a dart instead,
// its third vertex pushed in towards the first.
export const spellFigure = (shapeId: string, points: number[], variant?: string): [number, number][] => {
    if (shapeId === "quadrilateral" && variant === "Concave")
        return [[16, 5], [27, 27], [16, 19], [5, 27]];
    const n = Math.min(...points);
    const start = -Math.PI / 2 + (n % 2 === 0 ? Math.PI / n : 0);
    return Array.from({ length: n }, (_, i): [number, number] => {
        const a = start + (i * 2 * Math.PI) / n;
        return [Math.round(16 + 11 * Math.cos(a)), Math.round(16 + 11 * Math.sin(a))];
    });
};
