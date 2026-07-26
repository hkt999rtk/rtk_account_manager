FROM golang:1.24-bookworm AS builder

WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/rtk-account-manager ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/rtk-account-manager-migrate ./cmd/migrate
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/rtk-account-manager-email-worker ./cmd/email-worker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/rtk-account-manager-email-outbox-admin ./cmd/email-outbox-admin

FROM debian:bookworm-slim

WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 app \
    && chown app:app /app

COPY --from=builder /out/rtk-account-manager /app/rtk-account-manager
COPY --from=builder /out/rtk-account-manager-migrate /app/rtk-account-manager-migrate
COPY --from=builder /out/rtk-account-manager-email-worker /app/rtk-account-manager-email-worker
COPY --from=builder /out/rtk-account-manager-email-outbox-admin /app/rtk-account-manager-email-outbox-admin
COPY --from=builder /src/migrations /app/migrations

USER app
EXPOSE 8080
ENTRYPOINT ["/app/rtk-account-manager"]
