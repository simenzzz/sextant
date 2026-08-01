# ---- build ----
FROM node:22-alpine AS build
WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY . .
# Vite inlines VITE_* variables at BUILD time, not at container start, so this
# has to be an ARG here rather than an ENV in the runtime stage. Changing the
# API URL means rebuilding the image.
ARG VITE_API_URL=http://localhost:8080
ENV VITE_API_URL=$VITE_API_URL
RUN npm run build

# ---- serve ----
# Pinned to a minor line, not bare `alpine`: `nginx:alpine` floats across major
# versions, so an image that builds clean today can serve a different nginx
# next month with no diff in the repo.
#
# The unprivileged variant runs as uid 101 with no root anywhere, matching the
# other two images. Stock nginx runs its master process as root purely to bind
# port 80, which this does not need.
FROM nginxinc/nginx-unprivileged:1.27-alpine
# Templates in this directory are rendered by the entrypoint at container
# start, so API_ORIGIN — which the CSP needs — can differ per environment
# without rebuilding the image.
COPY --chown=nginx:nginx default.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build --chown=nginx:nginx /app/dist /usr/share/nginx/html
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget --quiet --spider http://localhost:8080/ || exit 1
