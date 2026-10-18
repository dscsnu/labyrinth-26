/**
 * The client's scan countdown. The backend accepts a pattern whose scans span
 * at most scan_window + scan_grace; the client only lets players scan for
 * scan_window, so the grace absorbs network latency and players never hit
 * the server's "too late" case.
 *
 * Rule (mirrored by the backend's simulation tests, internal/server/
 * tests/simulation_test.go — keep them in step):
 * - the countdown starts when this client first sees a non-empty stack: its
 *   own 200 for the first scan, or a teammate's scan arriving as a
 *   team_state event
 * - it resets whenever the client sees an empty stack (a new selection,
 *   a completed pattern)
 * - once scan_window has passed, the client sends no more scans; the team
 *   selects again to start over
 *
 * Times are milliseconds from any monotonic clock (performance.now()).
 */
export class ScanGate {
    private startedAt: number | null = null;

    constructor(private readonly windowMs: number) {}

    /** Updates the gate with the size of a stack the client just learned of. */
    observe(stackSize: number, now: number): void {
        if (stackSize === 0) this.startedAt = null;
        else if (this.startedAt === null) this.startedAt = now;
    }

    /** Whether the client may send a scan now. */
    canScan(now: number): boolean {
        return this.startedAt === null || now - this.startedAt < this.windowMs;
    }

    /** Milliseconds left on the countdown, or null when none is running. */
    remainingMs(now: number): number | null {
        if (this.startedAt === null) return null;
        return Math.max(0, this.windowMs - (now - this.startedAt));
    }
}
