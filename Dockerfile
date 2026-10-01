# The build stage runs on the builder's own platform and cross-compiles, so
# multi-arch images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev TARGETOS TARGETARCH
RUN export CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
 && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/omni-proxy ./cmd/omni-proxy \
 && go build -trimpath -ldflags="-s -w" -o /out/fake-upstream ./cmd/fake-upstream

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source=https://github.com/omni-proxy/omni-proxy \
      org.opencontainers.image.licenses=MIT
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/omni-proxy"]
CMD ["-config", "/etc/omni-proxy/config.yaml"]
