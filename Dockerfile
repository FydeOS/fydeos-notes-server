# Copyright (c) 2026 Fyde Innovations Limited
# SPDX-License-Identifier: MIT
#
# Backend-only image. Mount a built web app at /app/static to serve it from
# the same container, or serve the frontend separately.

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=development
ARG REVISION=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.releaseVersion=${VERSION} -X main.buildRevision=${REVISION}" \
      -o /out/fydeos-notes-server ./cmd/fydeos-notes-server \
 && go run github.com/google/go-licenses/v2@v2.0.1 save ./cmd/fydeos-notes-server \
      --save_path=/out/third_party_licenses --force

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --home-dir /var/lib/notes notes
WORKDIR /app
COPY --from=build /out/fydeos-notes-server /app/fydeos-notes-server
COPY --from=build /out/third_party_licenses /app/licenses/third_party
COPY LICENSE NOTICE /app/licenses/
USER notes
ENV NODE_ENV=production
EXPOSE 3010
ENTRYPOINT ["/app/fydeos-notes-server"]
