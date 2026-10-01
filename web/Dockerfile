# The bundle is static, so build it on the builder's own platform.
FROM --platform=$BUILDPLATFORM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
LABEL org.opencontainers.image.source=https://github.com/omni-proxy/omni-proxy \
      org.opencontainers.image.licenses=MIT
# PROXY_URL is where nginx forwards /admin; the template is filled at startup.
ENV PROXY_URL=http://proxy:8080
COPY nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
