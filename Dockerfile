FROM golang:1.22.3-alpine as build-stage

RUN mkdir /app
COPY . /app
WORKDIR /app
RUN CGO_ENABLED=0 GOOS=linux go build -o odesair /app/

FROM alpine
COPY --from=build-stage /app/odesair /
CMD ["/odesair"]