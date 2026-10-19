/// <reference types="bun" />
import { describe, expect, test } from "bun:test";
import { ScanGate } from "./scanGate.ts";
import { ServerClock } from "./serverClock.ts";

const WINDOW = 60_000;
const T0 = Date.parse("2026-10-17T12:00:00Z");

describe("ScanGate", () => {
    test("open with no countdown", () => {
        const g = new ScanGate(WINDOW);
        expect(g.canScan(T0)).toBe(true);
        expect(g.remainingMs(T0)).toBeNull();
    });

    test("the deadline is the bottom scan's time plus the window", () => {
        const g = new ScanGate(WINDOW);
        g.observe(T0);
        expect(g.remainingMs(T0)).toBe(WINDOW);
        expect(g.canScan(T0 + WINDOW - 1)).toBe(true);
        expect(g.canScan(T0 + WINDOW)).toBe(false);
        expect(g.remainingMs(T0 + WINDOW + 5)).toBe(0);
    });

    test("later scans don't move it; a new bottom (the old one went stale) does", () => {
        const g = new ScanGate(WINDOW);
        g.observe(T0); // more scans above the same bottom: same call
        expect(g.remainingMs(T0 + 50_000)).toBe(10_000);
        g.observe(T0 + 30_000); // the bottom went stale; the next scan is the bottom now
        expect(g.remainingMs(T0 + 50_000)).toBe(40_000);
    });

    test("an empty stack ends it (selection, cast, everything stale)", () => {
        const g = new ScanGate(WINDOW);
        g.observe(T0);
        g.observe(null);
        expect(g.canScan(T0 + WINDOW + 1)).toBe(true);
        expect(g.remainingMs(T0 + WINDOW + 1)).toBeNull();
    });

    test("a reloaded page gets the same deadline as everyone else", () => {
        const a = new ScanGate(WINDOW);
        const b = new ScanGate(WINDOW); // a teammate who reloaded 40 s in
        a.observe(T0);
        b.observe(T0);
        expect(b.remainingMs(T0 + 40_000)).toBe(a.remainingMs(T0 + 40_000));
    });
});

describe("ServerClock", () => {
    test("uses the device's clock until the first hello", () => {
        let local = 1_000;
        const c = new ServerClock(() => local);
        expect(c.synced).toBe(false);
        expect(c.now()).toBe(1_000);
        local = 2_500;
        expect(c.now()).toBe(2_500);
    });

    test("corrects a device clock that runs fast or slow", () => {
        let local = T0 + 7_000; // device 7 s fast
        const c = new ServerClock(() => local);
        c.hello(new Date(T0).toISOString());
        expect(c.synced).toBe(true);
        expect(c.now()).toBe(T0);
        local += 1_500;
        expect(c.now()).toBe(T0 + 1_500);
    });

    test("each hello re-measures the offset (a reconnect)", () => {
        let local = T0;
        const c = new ServerClock(() => local);
        c.hello(new Date(T0 + 3_000).toISOString()); // device 3 s slow
        expect(c.now()).toBe(T0 + 3_000);
        local = T0 + 10_000;
        c.hello(new Date(T0 + 10_000).toISOString()); // now in step
        expect(c.now()).toBe(T0 + 10_000);
    });

    test("a late hello makes the estimate run behind, never ahead", () => {
        // The server stamps hello at T0; it reaches the device 200 ms later.
        const local = T0 + 200;
        const c = new ServerClock(() => local);
        c.hello(new Date(T0).toISOString());
        expect(c.now()).toBe(T0); // the server is really at T0 + 200: behind by the delay
    });
});
