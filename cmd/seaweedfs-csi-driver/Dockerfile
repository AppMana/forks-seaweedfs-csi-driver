ARG PAYLOAD_STAGE=builder
FROM golang:1.25-alpine as builder

RUN apk add git g++

WORKDIR /go/src/github.com/seaweedfs/seaweedfs-csi-driver
COPY . .

RUN go build -ldflags="-s -w" -o /seaweedfs-csi-driver ./cmd/seaweedfs-csi-driver/main.go && go clean -cache -modcache

FROM alpine AS prebuilt
ARG DRIVER_SHA256
COPY seaweedfs-csi-driver /seaweedfs-csi-driver
RUN test -n "$DRIVER_SHA256" && echo "$DRIVER_SHA256  /seaweedfs-csi-driver" | sha256sum -c -

FROM ${PAYLOAD_STAGE} AS payload
FROM alpine AS final
RUN apk add fuse
LABEL author="Chris Lu"
COPY --from=payload /seaweedfs-csi-driver /

RUN chmod +x /seaweedfs-csi-driver
ENTRYPOINT ["/seaweedfs-csi-driver"]
