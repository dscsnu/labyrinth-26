import getCookie from "#lib/utils/getCookie.ts";
import { browser } from "$app/env";
import { writable } from "svelte/store";

export const DEVICE_TOKEN_NAME = 'labyrinth-26-device';
const initialValue: Device = 'mobile';
const maxAgeSec = 365 * 24 * 60 * 60; // 1 year as seconds

const getInitialData = (): Device => {
    if (!browser) return initialValue;

    const cookieData = getCookie(document.cookie, DEVICE_TOKEN_NAME);
    if (cookieData) {
        return cookieData as Device;
    }

    return initialValue;
}


export const setDevice = (theme: Device): void => {
    if (!browser) return;

    document.documentElement.dataset.theme = theme;
    document.cookie = `${DEVICE_TOKEN_NAME}=${theme};path=/;max-age=${maxAgeSec};`;
    DeviceStore.set(theme);
}

export const DeviceStore = writable<Device>(getInitialData());