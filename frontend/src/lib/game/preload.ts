// Fetching and decoding images ahead of the moment they're shown, so they
// appear at once instead of popping in. Browser only.
import mapUrl from "#lib/assets/map.webp";
import coinsUrl from "#lib/assets/MapIcons/coins.png";
import glowUrl from "#lib/assets/MapIcons/glow.png";
import glowPulseUrl from "#lib/assets/MapIcons/glow_pulse.png";

// One load per URL. The Image is kept, so the decoded pixels stay in memory;
// a failed load is forgotten, so the next call tries again.
const images = new Map<string, Promise<HTMLImageElement>>();

// Resolves once url is downloaded and decoded.
export const loadImage = (url: string): Promise<HTMLImageElement> => {
    let p = images.get(url);
    if (p) return p;
    p = new Promise<HTMLImageElement>((resolve, reject) => {
        const img = new Image();
        img.onload = () => resolve(img);
        img.onerror = () => reject(new Error(`${url} failed to load`));
        img.src = url;
        // decode() also waits for the pixels; some browsers reject it for
        // images they can still show, so onload stands in then.
        img.decode().then(() => resolve(img), () => {});
    });
    p.catch(() => images.delete(url));
    images.set(url, p);
    return p;
};

// Runs fn when the browser has nothing better to do.
export const whenIdle = (fn: () => void) => {
    if ("requestIdleCallback" in window) requestIdleCallback(fn);
    else setTimeout(fn, 200);
};

// The map and its markers' sheets (MapView's), the glows included: they're
// CSS backgrounds, only fetched the first time a point is scanned otherwise.
export const MAP_ART = [mapUrl, coinsUrl, glowUrl, glowPulseUrl];
export const preloadMapArt = () => Promise.all(MAP_ART.map(loadImage));
