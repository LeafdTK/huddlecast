FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/huddlecast ./cmd/huddlecast

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium xvfb fonts-liberation fonts-noto-color-emoji ca-certificates tini curl \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -m -u 10001 huddlecast
COPY --from=build /out/huddlecast /usr/local/bin/huddlecast
USER huddlecast
WORKDIR /home/huddlecast
ENV HUDDLECAST_CONFIG=/config/huddlecast.yml HUDDLECAST_NO_SANDBOX=1
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["sh", "-c", "huddlecast serve --config ${HUDDLECAST_CONFIG}"]
