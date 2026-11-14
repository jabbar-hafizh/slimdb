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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/slimdb /usr/local/bin/slimdb
COPY --from=build --chown=nonroot:nonroot /out/data /data

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
