# gowebp 功能完善 Roadmap

本文件規劃如何讓 `gowebp`（`github.com/KarpelesLab/gowebp`）的功能更趨完整。
聚焦三個主軸：**Metadata 與標準符合**、**API 易用性**、**壓縮品質**。
效能與安全的規劃另見 [`security.md`](./security.md)。

> 文件慣例：說明用台灣正體中文，型別／函式／chunk 等技術名詞保留英文。

---

## 1. 現況盤點

| 面向 | 現況 | 主要位置 |
|---|---|---|
| 無損編碼 | VP8L 完整（predict / subtract-green / color / palette transforms） | `writer.go`、`transform.go`、`huffman.go` |
| 有損編碼 | VP8 intra-only keyframe（I16 + B_PRED + UV8、DCT/WHT、boolean coder、loop filter、reconstruction-aware RDO） | `internal/vp8enc/` |
| 動畫 | ANIM/ANMF，兩種 codec 皆可 | `writer.go`（`EncodeAll`） |
| Alpha | ALPH chunk（raw 或 VP8L 壓縮，static / in-animation） | `writer.go`、`internal/vp8enc/yuv.go` |
| 解碼 | 委派 `golang.org/x/image/webp` + BT.601 修正 + alpha-flag 修正 | `reader.go:38-113` |
| 公開 API | `Encode`、`EncodeAll`、`Decode`、`DecodeConfig`、`DecodeIgnoreAlphaFlag` | `writer.go` / `reader.go` |

`Options`（`writer.go:39-44`）目前僅有四個欄位：`UseExtendedFormat`、`Lossy`、`Quality`、`Method`。

---

## 2. 缺口分析

| 類別 | 缺口 | 證據 |
|---|---|---|
| 標準符合 | VP8X 宣告支援 metadata，但 ICCP / EXIF / XMP chunk 從未實際寫入 | `writer.go:67-71` 註解提到能力，無對應寫入碼 |
| API | 無損路徑沒有 effort 旋鈕（`Method` 僅作用於 lossy）；無 `Exact`；無取消機制 | `Options`（`writer.go:39-44`）；`Encode` 簽章 `writer.go:82` |
| 壓縮品質 | VP8L 全圖只用單一 Huffman group；color cache bits 固定；predictor 未逐塊擇優 | `computeHistograms`（`writer.go:699`）；`transform.go:224` 既有 TODO |
| 壓縮品質 | VP8 缺校準 λ 的 RDO 與 Viterbi trellis | `README.md:202-204` 已列為 remaining work |

---

## 3. Phase A — Metadata 與標準符合（低風險，優先）

**目標**：讓 VP8X 容器名實相符，能攜帶 ICC profile、EXIF、XMP。

**設計**
- `Options` 新增三個欄位：
  ```go
  ICCProfile []byte // ICCP chunk（存在時自動帶起 VP8X）
  EXIF       []byte // EXIF chunk
  XMP        []byte // XMP chunk
  ```
- 任一 metadata 非空時，強制走 VP8X 容器（即使呼叫端未設 `UseExtendedFormat`）。
- 依 WebP 規範的 **RIFF chunk 順序** 寫入：

  ```
  RIFF/WEBP
    └─ VP8X (feature flags)
       ├─ ICCP        ← 必須在影像資料之前
       ├─ ANIM/ANMF 或 VP8/VP8L (+ALPH)
       ├─ EXIF        ← 影像資料之後
       └─ XMP
  ```
- 正確設定 VP8X feature flag bits：`ICC=0x20`、`Alpha=0x10`、`EXIF=0x08`、`XMP=0x04`、`Anim=0x02`。
  （目前 VP8X 寫入只設了動畫／alpha 相關旗標。）

**重用**
- 既有 VP8X 包裝路徑（`writer.go:87-96`）與 `bitwriter.go` 的 chunk 寫入。
- chunk 需 padding 至偶數位元組（RIFF 規定），確認既有寫入碼已處理。

**驗證**
- 以 `webpinfo` / `dwebp` / `exiftool` 確認 chunk 存在、順序正確、可被標準工具讀回。
- 新增 round-trip 測試：寫入 → 用上述工具或自寫 chunk parser 讀回比對。

---

## 4. Phase B — API 易用性（與 Phase A 共用 Options 擴充）

- **無損 effort 旋鈕**：讓 `Method`（或新增 `Effort`）在無損路徑也生效，控制：
  - 是否嘗試多個 color cache bits 取最佳；
  - predictor／color transform 的搜尋深度（呼應 Phase C）。
  目前 `Method` 僅影響 lossy（`README.md:83-91` 的 tier 表）。
- **`Options.Exact`**：保留全透明像素（A=0）的 RGB 值。目前 `flatten`（`writer.go:729-752`）直接複製 NRGBA，需確認透明像素 RGB 不被前處理改動；提供旗標讓使用者明確選擇「精確保留」vs「最佳壓縮」。
- **`context.Context` 取消**：新增 `EncodeContext` / `EncodeAllContext`（或在 `Options` 帶 `ctx`），於 MB 主迴圈（`internal/vp8enc/frame.go:266-274`）與 VP8L pixel 迴圈檢查取消。直接呼應 [`security.md`](./security.md) 的 DoS 緩解。
- **`image.Image` 慣用法**：decode 已透過 `image.RegisterFormat` 註冊（`reader.go:21-23`）；補強編碼端文件與便利範例（例如直接吃 `*image.Gray`、`*image.Paletted` 等，內部已用 `draw.Draw` 轉 NRGBA）。

---

## 5. Phase C — 壓縮品質（風險最高，需 PSNR 門檻守護）

- **VP8L meta-Huffman / entropy image**（最大無損收益）：
  目前整張影像共用單一 histogram（`computeHistograms`，`writer.go:699`）。
  改為將影像切成 block，建立 entropy image 與多組 Huffman code，讓不同區域用各自最佳的編碼表。
- **per-block predictor 擇優** 與 color transform：實作 `transform.go:224` 既有 TODO「analyze block and pick best Color transform Element (CTE)」，並讓 predictor transform 逐塊選最佳模式而非固定。
- **near-lossless** 前處理：在可接受誤差內量化掉不可感知細節以提升壓縮率（搭配 Phase B 的 effort 旋鈕開關）。
- **可調 color cache bits**：目前固定（`writeBitStreamData` 傳入定值）；改為可調並嘗試開／關取較小者。
- **VP8 有損**：
  - **segmentation**：最多 4 段自適應量化，依區域複雜度分配 quantizer。
  - **校準 λ 的 RDO** 與 **Viterbi trellis quantization**（`README.md:202-204`）。
- **守護**：所有改動以 `TestGalleryPSNR`（`gallery_test.go`）的 PSNR 門檻防止品質回退；無損改動以既有 round-trip 測試確保 byte-exact 還原。

---

## 6. 非目標（Non-goals）

- **原生 decoder**：暫不自寫 VP8/VP8L decoder，維持委派 `golang.org/x/image/webp`。
  理由：decode 是處理「不可信輸入」的主攻擊面，自寫需大量 fuzz 與 bounds 硬化（CVE-2023-4863 類風險）；委派成熟函式庫較安全。列為未來選項，相關安全考量見 [`security.md`](./security.md) Part 2 第 6 點。
  *備註*：若未來自寫 decoder，可順帶解決目前 `DecodeIgnoreAlphaFlag`（`reader.go:87`）的 workaround 與 metadata 讀取。
- **VP8 inter-frame / P-frame**：本專案定位為 intra-only keyframe 編碼器。

---

## 7. 里程碑與相依關係

```
Phase A (Metadata)  ──→  Phase B (API)  ──→  Phase C (壓縮品質)
  獨立、低風險           依賴 A 的             最複雜、需基準
  可先交付               Options 擴充          + PSNR 雙重守護
```

- **Phase A** 自成一體，無相依，建議先做、先發版。
- **Phase B** 共用 A 的 `Options` 擴充；`context` 取消同時服務於安全規劃。
- **Phase C** 風險最高，務必在每步前後跑 `TestGalleryPSNR` 與 round-trip 測試。
