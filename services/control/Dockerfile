# Control (Go orchestrator) — the ONLY DB client and sole decryptor.
FROM golang:1.26-alpine AS build
WORKDIR /src
# module files first for layer caching
COPY services/control/go.mod services/control/go.sum ./
RUN go mod download
COPY services/control/ ./
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/control ./cmd/control

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl && adduser -D -u 10001 control
USER control
COPY --from=build /out/control /usr/local/bin/control
# Health + workerlink ports (bound via env in compose).
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/control"]
