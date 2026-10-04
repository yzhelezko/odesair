FROM golang:1.27-alpine AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -ldflags="-w -s" -trimpath -o /odesair .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=build /odesair /odesair
CMD ["/odesair"]
