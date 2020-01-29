FROM alpine:edge

RUN apk add --no-cache go ca-certificates

WORKDIR /app

ADD cmd cmd/
ADD go.mod .
ADD go.sum .

RUN go build -o /usr/local/bin/rancher-conf ./cmd/rancher-conf

ENTRYPOINT [ "/usr/local/bin/rancher-conf" ]
