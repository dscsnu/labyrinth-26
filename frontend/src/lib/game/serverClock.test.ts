/// <reference types="bun" />
import { describe, expect, test } from "bun:test";
import { ServerClock } from "./serverClock.ts";

const T0 = Date.parse("2026-10-17T12:00:00Z");

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
