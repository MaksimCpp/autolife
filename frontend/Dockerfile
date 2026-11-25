FROM node:24.16.0-alpine@sha256:21f403ab171f2dc89bad4dd69d7721bfd15f084ccb46cdd225f31f2bc59b5c9a
ENV NEXT_TELEMETRY_DISABLED=1
WORKDIR /app
RUN corepack enable && chown node:node /app
USER node
COPY --chown=node:node package.json pnpm-lock.yaml ./
RUN --mount=type=cache,target=/pnpm-store,uid=1000,gid=1000 pnpm install --frozen-lockfile --store-dir=/pnpm-store
COPY --chown=node:node . .
ENV AUTOLIFE_API_ORIGIN=http://api:8080
RUN pnpm build
CMD ["pnpm", "start", "--hostname", "0.0.0.0"]
