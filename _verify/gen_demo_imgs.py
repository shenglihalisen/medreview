import os, math, random
from PIL import Image, ImageDraw, ImageFont, ImageFilter

OUT = r"D:\workbuddyprojets\2026-09-22-20-16-22\medreview\_verify\qc_demo_mat"
os.makedirs(OUT, exist_ok=True)
random.seed(42)
W, H = 1200, 800

def base_draw(label, fill_bg=(235, 235, 235)):
    img = Image.new("RGB", (W, H), fill_bg)
    d = ImageDraw.Draw(img)
    for x in range(0, W, 40):
        d.line([(x, 0), (x, H)], fill=(180, 180, 180), width=1)
    for y in range(0, H, 40):
        d.line([(0, y), (W, y)], fill=(180, 180, 180), width=1)
    for i in range(20):
        d.line([(random.randint(0, W), random.randint(0, H)),
                (random.randint(0, W), random.randint(0, H))],
               fill=(120, 120, 120), width=2)
    try:
        f = ImageFont.truetype("arial.ttf", 64)
    except Exception:
        f = ImageFont.load_default()
    d.text((40, 40), label, fill=(30, 30, 30), font=f)
    d.text((40, H - 120), "medreview QC demo", fill=(90, 90, 90), font=f)
    return img

# 1) 清晰对照
base_draw("SHARP / 清晰").save(os.path.join(OUT, "01_sharp.jpg"), quality=90)

# 2) 中度模糊：细密高对比细节（不会判空镜）+ 边缘柔化（拉普拉斯低 -> 触发模糊）
b = Image.new("RGB", (W, H), (230, 230, 228))
bd = ImageDraw.Draw(b)
random.seed(7)
for _ in range(900):
    x, y = random.randint(0, W - 1), random.randint(0, H - 1)
    s = random.randint(4, 18)
    col = (random.randint(20, 120), random.randint(20, 120), random.randint(20, 120))
    bd.ellipse([x, y, x + s, y + s], fill=col)
for _ in range(400):
    x1, y1 = random.randint(0, W - 1), random.randint(0, H - 1)
    x2, y2 = random.randint(0, W - 1), random.randint(0, H - 1)
    bd.line([(x1, y1), (x2, y2)], fill=(60, 60, 60), width=1)
try:
    f = ImageFont.truetype("arial.ttf", 56)
except Exception:
    f = ImageFont.load_default()
bd.text((40, 40), "BLUR / 模糊", fill=(20, 20, 20), font=f)
b = b.filter(ImageFilter.GaussianBlur(radius=7))
b.save(os.path.join(OUT, "02_blur.jpg"), quality=90)

# 3) 重度噪点
n = base_draw("NOISE / 噪点")
px = n.load()
for _ in range(60000):
    x, y = random.randint(0, W - 1), random.randint(0, H - 1)
    r, g, bl = px[x, y]
    d = random.randint(-90, 90)
    px[x, y] = (max(0, min(255, r + d)), max(0, min(255, g + d)), max(0, min(255, bl + d)))
n.save(os.path.join(OUT, "03_noise.jpg"), quality=90)

# 4) 过曝
o = Image.new("RGB", (W, H), (252, 252, 250))
od = ImageDraw.Draw(o)
od.text((40, 40), "OVER / 过曝", fill=(200, 200, 200), font=ImageFont.load_default())
o.save(os.path.join(OUT, "04_over.jpg"), quality=90)

# 5) 欠曝
u = Image.new("RGB", (W, H), (6, 6, 8))
u.save(os.path.join(OUT, "05_under.jpg"), quality=90)

# 6) 空镜
k = Image.new("RGB", (W, H), (128, 128, 128))
k.save(os.path.join(OUT, "06_blank.jpg"), quality=90)

print("generated:", sorted(os.listdir(OUT)))
