# The storage module as a static program in an image with nothing else in it: no shell, no package manager, a user that is not root, and nothing to write (the module keeps no state on a disk).
# renovate: datasource=docker depName=golang
ARG GO_VERSION=1.26.0
# renovate: datasource=docker depName=gcr.io/distroless/static
ARG RUNTIME_TAG=nonroot

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/blobgate ./cmd/blobgate

FROM gcr.io/distroless/static:${RUNTIME_TAG}
COPY --from=build /out/blobgate /blobgate
USER 65532:65532
EXPOSE 18090 18091
ENTRYPOINT ["/blobgate"]
