FROM golang:1.24-bookworm AS builder

WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/rtk-account-manager ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/rtk-account-manager-migrate ./cmd/migrate

FROM debian:bookworm-slim

WORKDIR /app
RUN useradd -r -u 10001 app && chown app:app /app

COPY --from=builder /out/rtk-account-manager /app/rtk-account-manager
COPY --from=builder /out/rtk-account-manager-migrate /app/rtk-account-manager-migrate
COPY --from=builder /src/migrations /app/migrations

USER app
EXPOSE 8080
ENTRYPOINT ["/app/rtk-account-manager"]
