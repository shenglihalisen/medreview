# 闭眼检测集成设计（路线 A：人脸关键点 ONNX + EAR）

> 状态：技术设计草案，待评审。
> 关联：`qc/qc.go` 现有 8 个纯像素统计检测器（损坏/重复/空镜/镜头脏点/模糊/欠曝/过曝/噪点/视频抖动）。

## 1. 目标与范围

- 在 medreview 现有 QC 管线中新增「闭眼 / eyes-closed」检测器，覆盖**人像 / 合影**场景（当前 8 个检测器均为场景/光学缺陷，无人像向）。
- 仅做检测 + 可视化标注（卡片徽章 + 可选灯箱红框），**绝不修改 `review.decision`**（与既有 QC 一致）。
- 纯本地推理，无云、无遥测。

## 2. 现状约束（必读，决定架构）

medreview 当前构建铁律（README 2.2）：

- 全部 QC 检测器为**纯像素统计**（`灰度标准差 → 空镜`、`拉普拉斯方差 → 模糊`、`亮度直方图 → 曝光`、`平坦块噪声 → 噪点`、`哈希 → 重复`），**零 ML 依赖**。
- 明确「**不使用 CGO，无需 gcc**」。
- 单 exe + `go:embed` 内嵌 ffmpeg。

⚠️ **关键张力**：主流 ONNX 推理（`yalue/onnxruntime_go`）**依赖 CGO + 原生 onnxruntime DLL**，直接与「不使用 CGO」冲突。必须在三条路线里选一条（本文默认 A1，但需你拍板）：

| 路线 | 说明 | 对「无 CGO / 单 exe」的影响 |
| --- | --- | --- |
| **A1（默认建议）** | onnxruntime_go（CGO）+ 内嵌 onnxruntime DLL | 需开启 CGO、随包带 ~30MB DLL；运行时仍是单 exe（DLL 内嵌释放，同 ffmpeg 机制） |
| A2 | 纯 Go 推理（onnx-go + Gorgonia） | 保持无 CGO，但算子支持有限、速度慢，人脸模型可能不兼容 |
| B | 外部 sidecar 进程（Go 编的小推理 exe） | 破坏单 exe，需管理子进程生命周期 |

**建议默认 A1**：开启 CGO 仅影响构建（需 gcc，本机已有），运行时仍是单 exe（DLL 内嵌释放）。但要把「无 CGO」铁律改为「**仅在含闭眼检测的构建标签下启用 CGO**」。

## 3. 模型选型

- **人脸检测**：轻量 ONNX（yolov5s-face / retinaface-tiny / mediapipe face-detection ONNX 导出），输出 bbox。
- **关键点**：68 点人脸关键点 ONNX（HRNet / 2D-FAN 等导出），只需其中**每只眼 6 个关键点**。比 MediaPipe 468 点更轻。
- 模型体积：检测 ~5MB + 关键点 ~10MB，可 `go:embed` 进二进制（同 ffmpeg 内嵌机制）。
- 推理后端：onnxruntime **CPU EP** 即可（人脸模型很轻）；可选 DirectML EP 走 Windows 独显/核显（不强制）。

## 4. 算法：EAR（Eye Aspect Ratio）

对每只眼取 6 个关键点（68 点标准索引）：

- 水平两点（眼外角 p1 / 眼内角 p4）定义眼宽；
- 垂直两点（上眼睑 p2 / 下眼睑 p6）定义开口度。

```
EAR = ‖p2 - p6‖ / ‖p1 - p4‖
```

- 睁眼时 EAR ≈ 0.25–0.35；闭眼时趋近 0。
- 双眼 EAR 均 < 阈值 → 判定闭眼。`Conf` 可取平均 EAR 的倒数或低值强度，越低越确定。

## 5. 管线集成

复用现有 QC 帧抽取（`startFrameStream`）：

1. **图片**：取单帧。
2. **视频**：抽 N 帧（复用现有 2fps 流或每秒 1 帧）；逐帧算 EAR，**时序聚合**避免把正常眨眼误判：
   - 仅当「闭眼帧占比 > 阈值（如 70%）」才标闭眼（区分「抓拍闭眼」vs「全程闭眼废片」）。
3. **人脸缺失** → 跳过（不标、不报错）；**多脸** → 逐脸判定，任一闭眼即标（或标最突出那张脸）。
4. 输出 `Blink *DetectorOut{Conf}`，并入 `QCResult`。

## 6. 需改动的文件

- 新增 `qc/blink.go`：模型会话加载、帧 → bbox → landmark → EAR、时序聚合。
- 改 `qc/qc.go`：`QCResult` 加 `Blink *DetectorOut`；`RunDetection` 串联。
- 新增 `qc/blink_cal.go` + 校准脚本：真实人像集上算 EAR 分布，按「宁漏勿滥 + 2–3× 余量」定阈值（初值 EAR<0.2，校准后取更保守值如 <0.15）。
- 新增 `tools/models/`：人脸检测 + 关键点 ONNX（`.gitignore` 排除，构建时 `go:embed` 打进二进制，运行时释放到 `%LocalAppData%\medreview\tools\models\`，同 `ffmpeg_embed.go` 机制）。
- 扩展 `ffmpeg_embed.go` 同款逻辑到模型 DLL + ONNX。
- 前端 `web/app.js`：`.qc-stack` 加「闭眼」徽章；可选灯箱红框圈眼睛（复用 boxes 机制，新加 `blinkBoxes`）。
- `zip.go` 的 `qcIssueTexts` / 问题清单 CSV 加「闭眼」。

## 7. 校准与验收

- **校准集**：≥50 张真实人像（睁眼 / 闭眼 / 侧脸 / 多人），含不同人种、年龄、光照。
- **验收**（沿用 `_verify/` 套路，自造素材 + 构建产物跑）：
  - 明显闭眼 → 标中；正常睁眼 → 不误标（宁漏勿滥）。
  - 视频连续闭眼 → 标中；单帧眨眼 → 不误标。
  - 无人脸 → 跳过无崩溃；多脸 → 逐脸正确。
  - 构建仍为 Windows 单 exe；CGO 仅影响编译环境，不影响运行形态。

## 8. 风险与开放问题

- **CGO 铁律冲突（最重要）**：需你确认是否接受「含闭眼构建标签下启用 CGO」。
- 模型许可：人脸/关键点 ONNX 许可证（多为 MIT/Apache，需逐一核对）。
- 侧脸 / 低头 EAR 失真：靠阈值保守 + 校准缓解。
- 体积：二进制 +~40MB（DLL + 模型）。

## 9. 工作量

约 **1–2 周**（含模型获取/导出、CGO 构建打通、校准脚本、前端徽章）。EAR 算法本身半天可写。

## 10. 备选增强（若已铺 ML 基建）

一旦引入人脸模型，可顺带做**人脸检测型**检测器（无人脸 / 脸太小 / 脸偏出框 / 多人脸），ROI 比单做闭眼更高，建议一并规划。
