FROM golang:1.24-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /ip-inject-fn .

FROM gcr.io/distroless/static-debian12
COPY --from=builder /ip-inject-fn /ip-inject-fn
USER 65532
ENTRYPOINT ["/ip-inject-fn"]
