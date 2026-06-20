FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/event-service ./cmd/event-service/main.go


FROM alpine:latest

RUN apk --n0-cache add ca-certificates

WORKDIR /app

COPY --from=builder /app/event-service .

EXPOSE 9091

CMD ["./event-service"]
