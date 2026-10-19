/**
 * The client's estimate of the server's clock: the one way the frontend
 * measures time against the game. Every event stream opens with
 * `hello {server_time}`; on each one, the offset between the server's clock
 * and this device's is re-measured, so every "how long ago" uses server time
 * and teammates' phones agree however wrong their own clocks are. (The game
 * has no time limit; time is only displayed.)
 *
 * Accuracy: hello reaches the device one network delay after the server
 * stamped it, so the estimate runs behind the server by that delay, never
 * ahead.
 *
 * Until the first hello, the offset is 0 (the device's own clock).
 */
export class ServerClock {
    private offsetMs = 0;
    private measured = false;

    /**
     * @param localNow the device's clock in epoch milliseconds; by default
     * performance.timeOrigin + performance.now(), which never jumps when the
     * user or the network changes the wall clock.
     */
    constructor(private readonly localNow: () => number = () => performance.timeOrigin + performance.now()) {}

    /** Re-measures the offset from a hello event's server_time (ISO 8601). */
    hello(serverTime: string): void {
        this.offsetMs = Date.parse(serverTime) - this.localNow();
        this.measured = true;
    }

    /** The server's current time, estimated, in epoch milliseconds. */
    now(): number {
        return this.localNow() + this.offsetMs;
    }

    /** Whether a hello has been seen yet (before it, now() is the device's clock). */
    get synced(): boolean {
        return this.measured;
    }
}
