# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd/shadowbrain-comment-bridge ./cmd/shadowbrain-comment-bridge
COPY internal ./internal

RUN test "$TARGETOS" = linux \
    && CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
       go build -trimpath -ldflags="-s -w" \
       -o /out/shadowbrain-comment-bridge ./cmd/shadowbrain-comment-bridge

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 shadowbrain \
    && useradd --system --uid 10001 --gid 10001 \
       --home-dir /home/shadowbrain --create-home shadowbrain \
    && install -d -m 0700 -o 10001 -g 10001 /var/lib/shadowbrain \
    && install -d -m 0755 -o root -g root /run/shadowbrain/secrets

WORKDIR /app

COPY --from=build --chown=root:root /out/shadowbrain-comment-bridge /app/shadowbrain-comment-bridge

ENV HOME=/tmp/shadowbrain \
    TZ=Asia/Tokyo

USER 10001:10001

STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/shadowbrain-comment-bridge"]
CMD ["run"]
