FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w ${VERSION:+-X wen/internal/version.Version=$VERSION}" \
    -o /out/wen ./cmd/wen

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 wen \
 && adduser -D -u 10001 -G wen wen \
 && mkdir -p /data \
 && chown wen:wen /data

COPY --from=build /out/wen /usr/local/bin/wen
COPY LICENSE /licenses/LICENSE

LABEL org.opencontainers.image.title="Wen Agent" \
      org.opencontainers.image.source="https://github.com/lprensoft/wen-agent" \
      org.opencontainers.image.licenses="MIT"

USER wen
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["wen"]
CMD ["serve", "-c", "/data/config.yaml", "-p", "8080", "-host", "0.0.0.0"]
