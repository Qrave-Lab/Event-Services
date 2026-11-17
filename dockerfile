FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/event-service ./cmd/server


FROM alpine:3.20

RUN apk --no-cache add ca-certificates && adduser -D -H -u 10001 app

WORKDIR /app

COPY --from=builder /app/event-service .

EXPOSE 9091

USER app

CMD ["./event-service"]
