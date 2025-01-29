FROM golang:1.22.3-alpine as build-stage

RUN mkdir /app
COPY . /app
WORKDIR /app
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -o odesair /app/

FROM alpine
COPY --from=build-stage /app/odesair /
CMD ["/odesair"]