# One image for every subcommand: the Deployment/Job picks api, worker or migrate as args.
# No BuildKit-only syntax (cache mounts etc.): the Jenkins agent builds with the legacy builder.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${VERSION}" \
      -o /out/marpokat ./cmd/marpokat

# Static ffmpeg, for the poster frame and the duration of videos (the worker). Without it videos are kept as they are,
# without thumbnail. Not ffprobe: it is another 150 MB and ffmpeg itself tells the duration.
FROM mwader/static-ffmpeg:7.1 AS ffmpeg

# distroless/static brings CA certificates (FCM, MinIO over TLS) and has no shell.
# UID 10001 matches the other services in the cluster (securityContext runAsUser).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/marpokat /usr/local/bin/marpokat
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
ENV FFMPEG_PATH=/usr/local/bin/ffmpeg
USER 10001:10001
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/marpokat"]
CMD ["api"]
