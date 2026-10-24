import type { Handle } from "@sveltejs/kit/hooks";

// Preloads the fonts the page's CSS uses (Kongtext, Superfly) along with the
// default JS and CSS, so they're fetched alongside the HTML rather than
// after the CSS is parsed, and are ready by the first paint.
const preloadFonts: Handle = ({ event, resolve }) =>
    resolve(event, {
        preload: ({ type }) => type === 'js' || type === 'css' || type === 'font',
    });

export default preloadFonts;
