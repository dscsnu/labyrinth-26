/**
 * Returns `next` if it's a same-site path like "/team?x=1", otherwise "/".
 * Rejects "//host", "/\host" and absolute URLs to prevent open redirects.
 */
const safeNext = (next: string | null): string => {
    if (!next || !next.startsWith('/') || next.startsWith('//') || /[\\\r\n\t]/.test(next)) {
        return '/';
    }
    return next;
}

export default safeNext;