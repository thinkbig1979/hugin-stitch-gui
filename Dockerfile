# Build the server. CGO is off, so the binary is static and the runtime stage
# needs no Go toolchain and no C libraries.
FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY static ./static
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hugin-stitch-gui .

FROM debian:bookworm-slim

# Hugin toolchain, ExifTool for metadata transfer, LibRaw for RAW decoding and
# libheif for HEIC, which is what an iPhone shoots by default.
RUN apt-get update && apt-get install -y --no-install-recommends \
        hugin-tools \
        enblend \
        libimage-exiftool-perl \
        libraw-bin \
        libheif-examples \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/hugin-stitch-gui /usr/local/bin/hugin-stitch-gui

# The UI is embedded in the binary, so nothing else needs to be copied.
ENV HOST=0.0.0.0
ENV PORT=8765
# How long a finished stitch is kept. 0s keeps every job until the server stops.
ENV RETENTION=2h

EXPOSE 8765

# Reports serving, not engine readiness: a missing Hugin tool is a broken
# image rather than something a restart would fix.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["perl", "-MIO::Socket::INET", "-e", "my $s = IO::Socket::INET->new(PeerAddr => '127.0.0.1', PeerPort => $ENV{PORT} || 8765, Timeout => 3) or exit 1; print $s \"GET /health HTTP/1.0\\r\\nHost: localhost\\r\\n\\r\\n\"; my $r = <$s>; exit($r =~ / 200 / ? 0 : 1);"]

ENTRYPOINT ["hugin-stitch-gui"]
