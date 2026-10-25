// The QR points marked on the map. Edit this file, not the map, to move,
// rename, add or remove one.

// map.webp's size in its own pixels. Point positions are in these pixels,
// from (0, 0) at the image's top-left corner; if the map image is replaced
// with one of another size, update this and re-pick the points.
export const MAP_SIZE = { width: 1168, height: 1346 };

export type MapPoint = {
    // Shown to screen readers; must be unique (it keys the marker).
    name: string;
    // Where the marker's tip touches, in map.webp's pixels (a cursor's
    // position in an image editor).
    x: number;
    y: number;
    // The backend waypoint this point is: the 4-digit ID its QR code
    // prints (2001-2010, backend seeds/2026.sql). Backend updates name
    // waypoints by it (e.g. team_state.stack[].waypoint_id), so it's how
    // they find their marker.
    waypoint: string;
};

export const MAP_POINTS: MapPoint[] = [
    { name: "G Block", x: 517, y: 222, waypoint: "2001" },
    { name: "A Block", x: 1019, y: 186, waypoint: "2009" },
    { name: "Arcade", x: 87, y: 440, waypoint: "2002" },
    { name: "Canopies", x: 515, y: 528, waypoint: "2008" },
    { name: "CnD Atrium", x: 986, y: 577, waypoint: "2010" },
    { name: "Atal Incubation Center", x: 533, y: 881, waypoint: "2006" },
    { name: "Lake Road", x: 991, y: 971, waypoint: "2007" },
    { name: "Amphi", x: 768, y: 1076, waypoint: "2005" },
    { name: "Chess Garden", x: 352, y: 1273, waypoint: "2004" },
    { name: "Inner gate", x: 58, y: 924, waypoint: "2003" },
];

// Rename rule for checkpoint names: maps raw/backend names to display names.
export const renameCheckpoint = (name: string): string => {
    if (name === "Lake") return "Lake Road";
    return name;
};
