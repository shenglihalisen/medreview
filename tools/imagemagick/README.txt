ImageMagick 放置说明（medreview 扩展格式支持用）
================================================

本目录已捆绑 ImageMagick 7.1.2-32 portable-Q16-x64（magick.exe + 配置 xml）。
该构建自带 heic / jxl / raw / openexr / jp2 / webp 等委托，能解码下方全部格式。

支持格式（ffmpeg 解不开、走 ImageMagick 预览）：
  - 相机 RAW：CR2 / CR3 / NEF / ARW / DNG / RAF / RW2 / ORF / SRW / PEF / MRW / ERF / SR2 / KDC / DCR / RWZ
  - 新图片格式：HEIC / HEIF / AVIF / JXL / PSD

说明：
  - 升级时：删掉本目录，换成新版本便携包里的 magick.exe + 全部 xml/icc/txt 配置即可。
  - 体积约 33MB（magick.exe 32MB + 配置）。
  - 便携分发时，把整个 tools/imagemagick/ 打进 zip，换台机器也能用。
  - 若 magick.exe 缺失/损坏，上述格式会退回「原文件」（浏览器多半打不开，但审阅页不崩），
    启动日志会提示 "图片预览: 未找到 ImageMagick"。
