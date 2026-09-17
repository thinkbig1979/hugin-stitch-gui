FROM python:3.12-slim

# Hugin toolchain + ExifTool for metadata transfer
RUN apt-get update && apt-get install -y --no-install-recommends \
        hugin-tools \
        enblend \
        libimage-exiftool-perl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .
COPY static ./static

ENV HOST=0.0.0.0
ENV PORT=8765

EXPOSE 8765

VOLUME /tmp/hugin-stitch-jobs

CMD ["sh", "-c", "python app.py ${PORT:-8765}"]