# The storage module as a static program in an image with nothing else in it: no shell, no package manager, a user that is not root, and nothing to write (the module keeps no state on a disk).
# The program is built on the machine that builds the image, for the machine the image is for (no emulation): `docker buildx build --platform linux/amd64,linux/arm64`.
# renovate: datasource=docker depName=golang
ARG GO_VERSION=1.26.0
# renovate: datasource=docker depName=gcr.io/distroless/static
ARG RUNTIME_TAG=nonroot

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/blobgate ./cmd/blobgate

FROM gcr.io/distroless/static:${RUNTIME_TAG}
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="blobgate" \
      org.opencontainers.image.description="The storage module of CiNim: blocks on any server rclone can talk to, mirrored, checked by SHA-256" \
      org.opencontainers.image.source="https://github.com/zystem/blobgate" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /out/blobgate /blobgate
USER 65532:65532
EXPOSE 18090 18091
ENTRYPOINT ["/blobgate"]
