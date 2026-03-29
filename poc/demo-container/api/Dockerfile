FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /api-svc .

FROM alpine:latest
COPY --from=builder /api-svc /usr/local/bin/api-svc
EXPOSE 8080
CMD ["api-svc"]
