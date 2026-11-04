# medreview 后端镜像（headless，前后端分离推荐）
#
#   docker build -t medreview:latest .
#   docker run -d --name medreview -p 8080:8080 \
#     -v /srv/photos:/media:ro -v medreview-data:/data medreview:latest
#
# 说明：
#  - 用 slim 标签构建（不内嵌 ffmpeg），运行时由 apt 提供 ffmpeg / ImageMagick；
#  - /media 只读挂载素材，/data 持久化数据库/缓存/日志；
#  - 对外暴露出反向代理时，务必加 TLS 与鉴权（见 docs/design/public-deployment.md）。

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -tags slim -trimpath -ldflags "-s -w" -o /out/medreview .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates ffmpeg imagemagick libheif1 libraw23 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/medreview /usr/local/bin/medreview

RUN useradd -r -u 10001 -m medreview && mkdir -p /data && chown -R medreview:medreview /data
USER medreview
EXPOSE 8080
VOLUME ["/media", "/data"]

ENTRYPOINT ["medreview"]
CMD ["-addr", ":8080", "-root", "/media", \
     "-db", "/data/medreview.db", "-cache", "/data/cache", "-log", "/data/medreview.log", \
     "-enc", "cpu", "-qcdec", "cpu"]
