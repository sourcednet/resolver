# sourced-resolver in a small image, running as a non-root user:
#   docker build -t sourced-resolver .
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN GOWORK=off go mod download
COPY . .
RUN GOWORK=off CGO_ENABLED=0 GOFLAGS=-mod=mod go build -trimpath -o /out/sourced-resolver ./cmd/sourced-resolver

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
	&& adduser -D -H -u 10001 sourced \
	&& mkdir /data && chown sourced /data
COPY --from=build /out/sourced-resolver /usr/local/bin/sourced-resolver
USER sourced
VOLUME /data
ENTRYPOINT ["sourced-resolver"]
