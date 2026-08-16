# syntax=docker/dockerfile:1.10
FROM node:24-bookworm-slim AS build
WORKDIR /src
COPY --from=frontend-source package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY --from=frontend-source . .
COPY --from=shared-source . /shared
RUN rm -rf /src/shared && ln -s /shared /src/shared
RUN npm test && npm run build

FROM nginxinc/nginx-unprivileged:1.29-alpine AS frontend
COPY --from=build --chown=nginx:nginx /src/dist /usr/share/nginx/html
USER nginx
EXPOSE 8080
