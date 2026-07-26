FROM golang:1.24-alpine AS builder

WORKDIR /app

# Download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o function .

# Final image
FROM alpine:3.21

RUN apk --no-cache add ca-certificates

WORKDIR /

COPY --from=builder /app/function .

USER 65532:65532

ENTRYPOINT ["/function"]
