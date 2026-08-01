# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src

# Copy manifests first so dependency download caches independently of source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off: modernc.org/sqlite is pure Go, so the result is a static binary that
# runs on a scratch-adjacent base with no libc to match.
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server

# ---- run ----
FROM alpine:3.21
# /data must exist AND be owned by the runtime user in the image. Docker
# initialises a fresh named volume from the image's directory, ownership
# included; if the directory is absent the mountpoint is created root-owned and
# uid 10001 cannot write its trace store, so the container crash-loops. The
# tempting fix — dropping USER — would hand the SQL-executing service root.
RUN adduser --system --uid 10001 sextant \
    && mkdir -p /data \
    && chown sextant /data
COPY --from=build /out/server /usr/local/bin/server
WORKDIR /data
USER sextant
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget --quiet --spider http://localhost:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/server"]
