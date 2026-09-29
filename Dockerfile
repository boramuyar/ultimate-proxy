FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ultimate-proxy ./cmd/ultimate-proxy \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-upstream ./cmd/fake-upstream

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ultimate-proxy"]
CMD ["-config", "/etc/ultimate-proxy/config.yaml"]
