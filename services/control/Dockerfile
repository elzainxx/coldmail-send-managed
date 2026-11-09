# Control (Go orchestrator) — the ONLY DB client and sole decryptor.
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
# module files first for layer caching
COPY services/control/go.mod services/control/go.sum ./
RUN go mod download
COPY services/control/ ./
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/control ./cmd/control

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates curl && adduser -D -u 10001 control
USER control
COPY --from=build /out/control /usr/local/bin/control
# Health + workerlink ports (bound via env in compose).
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/control"]
