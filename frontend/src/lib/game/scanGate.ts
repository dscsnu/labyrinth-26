/**
 * The client's scan countdown. The backend accepts a pattern whose scans span
 * at most scan_window + scan_grace, measured on its own clock from the bottom
 * (oldest live) scan; the client only lets players scan until
 * bottom.scanned_at + scan_window, on the server's clock as estimated by
 * ServerClock. The grace absorbs the one network delay that estimate can be
 * late by, plus the scan's own trip, so players never hit the server's "too
 * late" case.
 *
 * Rule (mirrored by the backend's simulation tests,
 * internal/server/tests/simulation_test.go — keep them in step):
 * - the deadline is the stack's bottom scan's scanned_at + scan_window, read
 *   from the latest team_state (or a scan's 200); every phone gets the same
 *   deadline, and reloading the page doesn't change it
 * - an empty stack (a new selection, a cast, every scan gone stale) means no
 *   countdown
 * - once the deadline has passed, the client sends no more scans; the team
 *   selects again to start over (or waits for the bottom scan to go stale)
 *
 * Times are epoch milliseconds on the server's clock (ServerClock.now()).
 */
export class ScanGate {
    private deadline: number | null = null;

    constructor(private readonly windowMs: number) {}

    /**
     * Updates the gate from the latest stack: bottomScannedAt is the bottom
     * scan's scanned_at in epoch milliseconds, or null for an empty stack.
     */
    observe(bottomScannedAt: number | null): void {
        this.deadline = bottomScannedAt === null ? null : bottomScannedAt + this.windowMs;
    }

    /** Whether the client may send a scan at serverNow. */
    canScan(serverNow: number): boolean {
        return this.deadline === null || serverNow < this.deadline;
    }

    /** Milliseconds left at serverNow, or null when no countdown is running. */
    remainingMs(serverNow: number): number | null {
        if (this.deadline === null) return null;
        return Math.max(0, this.deadline - serverNow);
    }
}
