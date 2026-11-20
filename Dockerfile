FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/server ./cmd/server
COPY internal ./internal
COPY tools/frpbuild ./tools/frpbuild
RUN CGO_ENABLED=0 go run ./tools/frpbuild build -trimpath -ldflags='-s -w' -o /out/multifrp ./cmd/server

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 multifrp \
    && mkdir -p /var/lib/multifrp \
    && chown 10001:10001 /var/lib/multifrp
COPY --from=build /out/multifrp /usr/local/bin/multifrp
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/multifrp"]
