import type { Attachment } from 'svelte/attachments';

/**
 * An attachment that sets an element's font size to the largest whole
 * pixel size, from `max` down to `min`, at which its text fits its width
 * on one line. The element should be `white-space: nowrap` (Tailwind's
 * `truncate` also keeps an ellipsis if even `min` is too big). On text
 * that wraps instead, it's the size at which its longest word fits, so
 * lines break only between words, unless a word is too long even at
 * `min`. It fits
 * again when its text changes, its container is resized, and once web
 * fonts have loaded, since they change the text's width.
 */
const fitText = ({ max, min }: { max: number; min: number }): Attachment<HTMLElement> => (node) => {
    const fits = (size: number) => {
        node.style.fontSize = `${size}px`;
        return node.scrollWidth <= node.clientWidth;
    };
    const fit = () => {
        node.style.overflowWrap = "";
        let lo = min;
        let hi = max;
        if (fits(hi)) return;
        // The largest size that fits, by halving [lo, hi].
        while (lo < hi) {
            const mid = Math.ceil((lo + hi) / 2);
            if (fits(mid)) lo = mid;
            else hi = mid - 1;
        }
        // A word too long even at min: wrapping text may break it, rather
        // than spill out (a no-op on nowrap text).
        if (!fits(lo)) node.style.overflowWrap = "anywhere";
    };

    fit();
    let alive = true;
    document.fonts?.ready.then(() => alive && fit());
    const text = new MutationObserver(fit);
    text.observe(node, { childList: true, characterData: true, subtree: true });
    const size = new ResizeObserver(fit);
    if (node.parentElement) size.observe(node.parentElement);

    return () => {
        alive = false;
        text.disconnect();
        size.disconnect();
    };
};

export default fitText;
