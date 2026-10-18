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

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /bin/api /bin/api

ENV APP_ENV=production \
    HTTP_HOST=0.0.0.0 \
    HTTP_PORT=8080 \
    LOG_LEVEL=info \
    LOG_FORMAT=json

EXPOSE 8080

ENTRYPOINT ["/bin/api"]
