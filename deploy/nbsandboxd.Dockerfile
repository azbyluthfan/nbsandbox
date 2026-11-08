# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# nbsandboxd service image. Build from the repository root:
#   docker build -f deploy/nbsandboxd.Dockerfile -t nbsandboxd:dev .

# golang:1.27-bookworm, pinned by multi-arch index digest.
FROM golang:1.27-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY gen ./gen
ARG VERSION=dev
# Pure Go: a static binary, no libc needed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/nbsandboxd ./cmd/nbsandboxd

# distroless static (debian13), pinned by multi-arch index digest.
FROM gcr.io/distroless/static-debian13@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/nbsandboxd /nbsandboxd
# nbsandboxd runs as uid 1000, the owner of the data directory.
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/nbsandboxd"]
