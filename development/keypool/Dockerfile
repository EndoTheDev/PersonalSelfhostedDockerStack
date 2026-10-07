# keypool — multi-stage build, arm64. CGO disabled: modernc.org/sqlite is pure Go.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/keypool ./cmd/keypool

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/keypool /keypool
VOLUME /data
ENV KEYPOOL_DB=/data/keypool.db
EXPOSE 11435
USER nonroot
ENTRYPOINT ["/keypool"]