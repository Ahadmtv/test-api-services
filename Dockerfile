# syntax=docker/dockerfile:1

FROM golang:1.27.0-bookworm AS builder

WORKDIR /src

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /bin/api ./cmd/api

# gcr.io is unreachable from this network (403); scratch + files from the
# builder reproduces distroless/static-debian12:nonroot closely enough.
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /bin/api /bin/api

USER 65534:65534

ENV APP_ENV=production \
    HTTP_HOST=0.0.0.0 \
    HTTP_PORT=8080 \
    LOG_LEVEL=info \
    LOG_FORMAT=json

EXPOSE 8080

ENTRYPOINT ["/bin/api"]
