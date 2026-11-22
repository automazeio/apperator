# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/apperator ./cmd/apperator

FROM alpine:3.22 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/apperator /apperator
ENV APPERATOR_DATA_DIR=/var/lib/apperator
VOLUME /var/lib/apperator
EXPOSE 7070
ENTRYPOINT ["/apperator"]
CMD ["serve"]
