# Bun installs and builds (bun.lock); Node runs the result, adapter-node's
# target. Nothing environment-specific is built in: every variable in
# src/env.js is read when the server starts, so one image runs anywhere.
FROM oven/bun:1.4.3-alpine AS deps
WORKDIR /app
COPY package.json bun.lock .npmrc ./
# Bun's package cache persists between builds (a BuildKit cache mount).
RUN --mount=type=cache,target=/root/.bun/install/cache bun install --frozen-lockfile

FROM deps AS build
COPY . .
# The build loads the server code, which checks every variable is set; these
# placeholders satisfy that and aren't kept (none is static: the build
# output has no trace of them).
RUN BACKEND_URL=http://build-placeholder.invalid JWT_PUBLIC_KEY=build-placeholder bun run build

# Only "dependencies": what build/ imports at run time. No scripts (prepare
# runs svelte-kit sync, which needs the sources).
FROM oven/bun:1.4.3-alpine AS prod-deps
WORKDIR /app
COPY package.json bun.lock .npmrc ./
RUN --mount=type=cache,target=/root/.bun/install/cache bun install --frozen-lockfile --production --ignore-scripts

FROM node:24-alpine
WORKDIR /app
# Root-owned, so the server can read and run its files but never change
# them. package.json is there for "type": "module".
COPY --from=prod-deps /app/node_modules ./node_modules
COPY --from=build /app/build ./build
COPY package.json ./
ENV NODE_ENV=production PORT=3000
EXPOSE 3000
# The image's unprivileged node user, numeric so a policy can tell it's not
# root without reading /etc/passwd. It writes no files and listens above 1024.
USER 1000:1000
# adapter-node finishes open requests on SIGTERM, then exits.
CMD ["node", "build"]
