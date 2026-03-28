FROM registry.redhat.io/ubi10/go-toolset:10.1 AS builder

WORKDIR /opt/app-root/src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 go build -o /opt/app-root/maas-tokenizer .

FROM registry.redhat.io/ubi10/ubi-minimal:latest

COPY --from=builder /opt/app-root/maas-tokenizer /usr/local/bin/maas-tokenizer

ENTRYPOINT ["maas-tokenizer"]
