# syntax=docker/dockerfile:1
#
# Multi-stage UI image with two entry points:
#   target: dev  → `next dev` with hot reload (used by the base docker-compose.yml)
#   target: prod → `next build` + `next start` production server (deploy overlays)
#
# NEXT_PUBLIC_* values are inlined into the client bundle at *build* time, so the
# browser's broker URL has to be supplied as a build arg for the prod target
# (the dev server reads it live from the environment instead). See lib/mqtt.ts.

# ── Shared dependency layer ──────────────────────────────────────────────
FROM node:20-bookworm-slim AS deps
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci

# ── Dev: hot-reloading server ────────────────────────────────────────────
FROM node:20-bookworm-slim AS dev
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
EXPOSE 3000
CMD ["npm", "run", "dev"]

# ── Build: compile the production bundle ─────────────────────────────────
FROM deps AS build
COPY . .
ARG NEXT_PUBLIC_MQTT_WS_URL
ENV NEXT_PUBLIC_MQTT_WS_URL=${NEXT_PUBLIC_MQTT_WS_URL}
RUN npm run build

# ── Prod: serve the compiled bundle ──────────────────────────────────────
FROM node:20-bookworm-slim AS prod
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/.next ./.next
COPY --from=build /app/package.json ./package.json
COPY --from=build /app/next.config.ts ./next.config.ts
EXPOSE 3000
CMD ["npm", "run", "start"]
