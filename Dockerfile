FROM ysicing/god AS builder

WORKDIR /go/src/github.com/ysicing/doh

COPY go.mod go.mod

COPY go.sum go.sum

RUN go mod download

COPY . .

RUN go install github.com/go-task/task/v3/cmd/task@latest

RUN task build

FROM ysicing/debian

COPY --from=builder /go/src/github.com/ysicing/doh/doh /usr/local/bin/doh

RUN chmod +x /usr/local/bin/doh

CMD [ "/usr/local/bin/doh" ]
