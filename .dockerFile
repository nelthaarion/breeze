FROM golang:alpine

ENV CGO_ENABLED=0 GOTOOLCHAIN=local

WORKDIR /app

COPY . .

RUN go mod download

RUN go build -o /app/api ./cmd/api-example

CMD ["/app/api"]