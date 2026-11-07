# Migration runner image. Built from the repo root (context .) so it can copy the module.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY tools/send-managed-migrate/go.mod tools/send-managed-migrate/go.sum ./
RUN go mod download
COPY tools/send-managed-migrate/ ./
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/send-managed-migrate .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/send-managed-migrate /usr/local/bin/send-managed-migrate
# db/ (migrations + preflight) is mounted at /db by compose.
ENTRYPOINT ["/usr/local/bin/send-managed-migrate"]
CMD ["up"]
