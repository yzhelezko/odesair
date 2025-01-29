FROM golang:1.22.3-alpine as build-stage

# Install build dependencies
RUN apk add --no-cache git

# Create app directory
WORKDIR /app

# Download dependencies first (this will be cached)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build with optimizations
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -ldflags="-w -s" \
    -trimpath \
    -o odesair /app/

# Final stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=build-stage /app/odesair /
CMD ["/odesair"]