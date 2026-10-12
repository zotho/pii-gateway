# Copyright (c) 2026 bitkaio LLC. All rights reserved.
# Licensed under the Apache License, Version 2.0. See LICENSE for details.
# Build with the latest patched Go (not just go.mod's 1.24 minimum): the
# compiled binary embeds the toolchain's stdlib, so an older Go bakes in
# stdlib CVEs that Trivy then flags. The 1.26-alpine tag tracks the newest
# 1.26 patch (Renovate keeps it current).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server -trimpath -ldflags="-s -w" ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
