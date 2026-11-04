# Образ сервиса: статический Go-бинарник без cgo на alpine. Метка revision = коммит:
# deploy.sh и bootstrap.sh отказывают, если она не равна целевому коммиту.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG BUILD=local
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/goslingmanagment/ledger/internal/version.Build=${BUILD}" \
    -o /out/ledger ./cmd/ledger

FROM alpine:3.22
RUN apk add --no-cache tini ca-certificates tzdata \
    && addgroup -g 1000 ledger && adduser -D -u 1000 -G ledger ledger \
    && mkdir -p /data && chown ledger:ledger /data
ARG BUILD=local
ENV LEDGER_DB=/data/ledger.db LEDGER_ADDR=:8080 LEDGER_BUILD=${BUILD} TZ=Europe/Moscow
LABEL org.opencontainers.image.revision=${BUILD}
COPY --from=build /out/ledger /usr/local/bin/ledger
USER ledger
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["ledger", "serve"]
