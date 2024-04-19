FROM golang:1.22-alpine as builder

ENV APP_HOME /go/src/irmgard
WORKDIR $APP_HOME

COPY . .

RUN go mod download
RUN go mod verify
RUN go build -ldflags="-s -w" -o irmgard

FROM alpine:3.19

ENV APP_HOME /go/src/irmgard
RUN mkdir -p "$APP_HOME"
WORKDIR $APP_HOME

COPY --from=builder $APP_HOME/irmgard $APP_HOME

EXPOSE 8080
CMD ["./irmgard"]