# SEAWEEDFS_COMMIT should match the commit in go.mod for reproducibility
# Update via: docker build --build-arg SEAWEEDFS_COMMIT=<hash> ...
ARG SEAWEEDFS_REPO=https://github.com/AppMana/forks-seaweedfs
ARG SEAWEEDFS_COMMIT=c66909247ef2cbae6beb69528ef10a71c7b38984
ARG GO_FUSE_REPO=https://github.com/AppMana/forks-go-fuse
ARG GO_FUSE_BRANCH=appmana/windows-compile

FROM golang:1.25-alpine AS builder

RUN apk add git g++

# Build weed binary from a specific commit for reproducibility. The
# AppMana fork's go.mod replaces go-fuse with a ../forks-go-fuse sibling
# checkout, and the production build is the large-disk (5BytesOffset)
# variant.
ARG SEAWEEDFS_REPO
ARG SEAWEEDFS_COMMIT
ARG GO_FUSE_REPO
ARG GO_FUSE_BRANCH
RUN git clone ${SEAWEEDFS_REPO} /go/src/github.com/seaweedfs/seaweedfs \
    && cd /go/src/github.com/seaweedfs/seaweedfs \
    && git checkout ${SEAWEEDFS_COMMIT} \
    && git clone --depth 1 -b ${GO_FUSE_BRANCH} ${GO_FUSE_REPO} /go/src/github.com/seaweedfs/forks-go-fuse \
    && cd weed \
    && go install -tags 5BytesOffset \
    && go clean -cache -modcache

# Build seaweedfs-mount from current context
WORKDIR /go/src/github.com/seaweedfs/seaweedfs-csi-driver
COPY . .

RUN go build -ldflags="-s -w" -o /seaweedfs-mount ./cmd/seaweedfs-mount/main.go && go clean -cache -modcache

FROM alpine AS final
RUN apk add fuse
LABEL author="Chris Lu"
COPY --from=builder /go/bin/weed /usr/bin/
COPY --from=builder /seaweedfs-mount /

RUN chmod +x /seaweedfs-mount
ENTRYPOINT ["/seaweedfs-mount"]
