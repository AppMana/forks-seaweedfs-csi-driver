ARG PAYLOAD_STAGE=builder
# Stay on the native build platform and cross-compile from there, instead of
# running the toolchain under QEMU once per target architecture.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /go/src/github.com/seaweedfs/seaweedfs-csi-driver

# Downloaded once and shared by all target platforms
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ENV CGO_ENABLED=0
RUN export GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    && go build -ldflags="-s -w" -o /seaweedfs-csi-driver ./cmd/seaweedfs-csi-driver/main.go

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
