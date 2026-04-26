FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY . .

RUN CGO_ENABLED=0 go build -o /service ./svc/crashtest

FROM alpine:3.21

RUN apk add --no-cache ca-certificates
COPY --from=builder /service /service

ENTRYPOINT ["/service"]
