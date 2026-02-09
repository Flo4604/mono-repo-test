FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY . .

ARG SERVICE=api
RUN CGO_ENABLED=0 go build -o /service ./svc/${SERVICE}

FROM alpine:3.21

RUN apk add --no-cache ca-certificates
COPY --from=builder /service /service

ENTRYPOINT ["/service"]
