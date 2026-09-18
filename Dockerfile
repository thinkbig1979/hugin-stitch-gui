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

# Hugin toolchain, ExifTool for metadata transfer, LibRaw for RAW decoding.
RUN apt-get update && apt-get install -y --no-install-recommends \
        hugin-tools \
        enblend \
        libimage-exiftool-perl \
        libraw-bin \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/hugin-stitch-gui /usr/local/bin/hugin-stitch-gui

# The UI is embedded in the binary, so nothing else needs to be copied.
ENV HOST=0.0.0.0
ENV PORT=8765

EXPOSE 8765

ENTRYPOINT ["hugin-stitch-gui"]
