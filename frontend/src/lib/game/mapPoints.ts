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
    // prints (e.g. "1001"). null until the backend's waypoints are final;
    // then fill these in to connect markers to the team's scans.
    waypoint: string | null;
};

export const MAP_POINTS: MapPoint[] = [
    { name: "G Block", x: 517, y: 222, waypoint: null },
    { name: "A Block", x: 1019, y: 186, waypoint: null },
    { name: "Arcade", x: 87, y: 440, waypoint: null },
    { name: "Cricket Turf", x: 515, y: 528, waypoint: null },
    { name: "CnD Atrium", x: 986, y: 577, waypoint: null },
    { name: "Atal Incubation Center", x: 533, y: 881, waypoint: null },
    { name: "Library Lawn Road", x: 991, y: 971, waypoint: null },
    { name: "Parking One", x: 768, y: 1076, waypoint: null },
    { name: "Chess Garden", x: 352, y: 1273, waypoint: null },
    { name: "Cricket Stadium", x: 58, y: 924, waypoint: null },
];
