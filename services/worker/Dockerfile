# Worker (Node.js SMTP relay skeleton) — NO DB, NO KMS. Pure mTLS client.
FROM node:24-alpine@sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8
RUN apk add --no-cache curl && adduser -D -u 10002 worker
WORKDIR /app
# package.json has NO dependencies (structural no-DB/no-KMS boundary); copy source only.
COPY services/worker/package.json ./
COPY services/worker/src ./src
USER worker
ENTRYPOINT ["node", "src/index.js"]
