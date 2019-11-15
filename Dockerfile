FROM golang AS builder

COPY . /go/src/github.com/artpar/rclone/
WORKDIR /go/src/github.com/artpar/rclone/

RUN make quicktest
RUN \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  make
RUN ./rclone version

# Begin final image
FROM alpine:latest

RUN apk --no-cache add ca-certificates fuse

COPY --from=builder /go/src/github.com/artpar/artpar/rclone /usr/local/bin/

ENTRYPOINT [ "rclone" ]

WORKDIR /data
ENV XDG_CONFIG_HOME=/config
