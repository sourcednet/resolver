# sourced-resolver in a small image, running as a non-root user.
#
# Until the sibling projects (core, …) are published, build it from Dev/,
# where go.mod's replace directives find them:
#   docker build -f resolver/Dockerfile -t sourced-resolver .
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY core ./core
COPY publisher ./publisher
COPY testkit ./testkit
COPY resolver ./resolver
WORKDIR /src/resolver
RUN CGO_ENABLED=0 go build -trimpath -o /out/sourced-resolver ./cmd/sourced-resolver

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
	&& adduser -D -H -u 10001 sourced \
	&& mkdir /data && chown sourced /data
COPY --from=build /out/sourced-resolver /usr/local/bin/sourced-resolver
USER sourced
VOLUME /data
ENTRYPOINT ["sourced-resolver"]
