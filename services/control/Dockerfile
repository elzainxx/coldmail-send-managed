# Control (Go orchestrator) — the ONLY DB client and sole decryptor.
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
# module files first for layer caching
COPY services/control/go.mod services/control/go.sum ./
RUN go mod download
COPY services/control/ ./
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/control ./cmd/control

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
RUN apk add --no-cache ca-certificates curl && adduser -D -u 10001 control
USER control
COPY --from=build /out/control /usr/local/bin/control
# Health + workerlink ports (bound via env in compose).
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/control"]
