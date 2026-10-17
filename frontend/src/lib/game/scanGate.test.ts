/// <reference types="bun" />
import { describe, expect, test } from "bun:test";
import { ScanGate } from "./scanGate.ts";

const WINDOW = 60_000;

describe("ScanGate", () => {
    test("open with no countdown", () => {
        const g = new ScanGate(WINDOW);
        expect(g.canScan(0)).toBe(true);
        expect(g.remainingMs(0)).toBeNull();
    });

    test("starts on the first non-empty stack and closes at the window", () => {
        const g = new ScanGate(WINDOW);
        g.observe(1, 1_000);
        expect(g.remainingMs(1_000)).toBe(WINDOW);
        expect(g.canScan(1_000 + WINDOW - 1)).toBe(true);
        expect(g.canScan(1_000 + WINDOW)).toBe(false);
        expect(g.remainingMs(1_000 + WINDOW + 5)).toBe(0);
    });

    test("later scans don't restart the countdown", () => {
        const g = new ScanGate(WINDOW);
        g.observe(1, 0);
        g.observe(2, 30_000);
        g.observe(3, 50_000);
        expect(g.remainingMs(50_000)).toBe(WINDOW - 50_000);
    });

    test("an empty stack resets it (selection or completion)", () => {
        const g = new ScanGate(WINDOW);
        g.observe(2, 0);
        g.observe(0, WINDOW + 1);
        expect(g.canScan(WINDOW + 1)).toBe(true);
        expect(g.remainingMs(WINDOW + 1)).toBeNull();
        g.observe(1, WINDOW + 10);
        expect(g.remainingMs(WINDOW + 10)).toBe(WINDOW);
    });

    test("a teammate's scan seen late starts this client's countdown late", () => {
        // This device learns of the first scan 300 ms after the server took
        // it; its window ends 300 ms later than the scanner's. The server's
        // grace covers that.
        const g = new ScanGate(WINDOW);
        g.observe(1, 300);
        expect(g.canScan(WINDOW + 299)).toBe(true);
        expect(g.canScan(WINDOW + 300)).toBe(false);
    });
});
