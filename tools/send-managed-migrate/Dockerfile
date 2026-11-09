# Migration runner image. Built from the repo root (context .) so it can copy the module.
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
COPY tools/send-managed-migrate/go.mod tools/send-managed-migrate/go.sum ./
RUN go mod download
COPY tools/send-managed-migrate/ ./
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/send-managed-migrate .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates
COPY --from=build /out/send-managed-migrate /usr/local/bin/send-managed-migrate
# db/ (migrations + preflight) is mounted at /db by compose.
ENTRYPOINT ["/usr/local/bin/send-managed-migrate"]
CMD ["up"]
