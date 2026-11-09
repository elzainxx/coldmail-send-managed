# Worker (Node.js SMTP relay skeleton) — NO DB, NO KMS. Pure mTLS client.
FROM node:20-alpine@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293
RUN apk add --no-cache curl && adduser -D -u 10002 worker
WORKDIR /app
# package.json has NO dependencies (structural no-DB/no-KMS boundary); copy source only.
COPY services/worker/package.json ./
COPY services/worker/src ./src
USER worker
ENTRYPOINT ["node", "src/index.js"]
