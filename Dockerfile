# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/slimdb ./cmd/slimdb \
 && mkdir -p /out/data

# Alpine rather than distroless: copying a Postgres database needs pg_dump
# and pg_restore. Version 18 dumps servers up to 18.
FROM alpine:3.23
RUN apk add --no-cache postgresql18-client ca-certificates tzdata \
 && adduser -D -H -u 65532 nonroot
COPY --from=build /out/slimdb /usr/local/bin/slimdb
# UID 65532 matches the earlier distroless image, so existing volumes stay readable.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532

# SLIMDB_HOME:            connections and history, mount a volume here
# SLIMDB_LISTEN_ANY:      listen on all interfaces inside the container;
#                         publish the port on 127.0.0.1 only
# SLIMDB_LOCALHOST_ALIAS: "localhost" in connection URLs means your machine,
#                         not the container
ENV SLIMDB_HOME=/data \
    SLIMDB_LISTEN_ANY=1 \
    SLIMDB_LOCALHOST_ALIAS=host.docker.internal

VOLUME /data
EXPOSE 7788
ENTRYPOINT ["slimdb"]
CMD ["serve", "-addr", "0.0.0.0:7788", "-no-browser"]
