# gowebp 效能與安全規劃

本文件規劃 WebP 轉換的 **效能提升**（Part 1）與 **安全硬化**（Part 2）。
功能完善的 roadmap 另見 [`plan.md`](./plan.md)。

**硬性約束**：效能方案維持**純 Go、不採用組語（`.s`）**，以保住專案「pure Go、no cgo」的可攜性賣點與單一實作的可維護性。

> 文件慣例：說明用台灣正體中文，型別／函式／符號等技術名詞保留英文。

---

## Part 1 — 效能（純 Go）

### Findings（現況）

- **無並行、無 SIMD**：編碼為單執行緒純 Go scalar。VP8 的 MB 主迴圈是 raster 串列（`internal/vp8enc/frame.go:266-274`）。
- **熱點**：
  - FDCT / IDCT / WHT / quant — `internal/vp8enc/dct.go`（每 MB 約 16 次 Y + 4 Cb + 4 Cr 個 4×4 transform）。
  - intra mode search 的 SSE — `SumSquaredError`（`internal/vp8enc/intra.go:150`），高 Method 下被反覆呼叫。
  - boolean arithmetic coder — `internal/vp8enc/boolcoder.go`（逐 coefficient 機率查表 + 進位處理）。
  - VP8L：transform 全圖配置（`transform.go`）與 LZ77 比對內迴圈（`writer.go:556` 起）。
- **無 `sync.Pool`**：per-frame 配置一次尚可，但 VP8L transform 有整圖 `deltas` / `blocks` copy。
- **benchmark 缺 `b.ReportAllocs()`**：`benchmark_test.go` 只量時間與體積，未追蹤配置壓力。

### Plan（依優先序）

1. **先量測，再優化**
   - `benchmark_test.go` 加 `b.ReportAllocs()`；加入更大尺寸測試圖；新增 per-kernel microbenchmark（單獨量 FDCT/IDCT/SSE/quant）。
   - 用 `pprof` 取 CPU / mem profile 定位真正熱點，建立改動前基線。**先有數字再動手。**

2. **並行（最大實務收益，純 Go）**
   - **跨 frame（低風險）**：`EncodeAll` 動畫各 frame 互相獨立，用 goroutine pool + `runtime.NumCPU()` 平行編碼，最後依序寫出。
   - **VP8L（中低風險）**：predictor / color transform 與 `computeHistograms`（`writer.go:699`）可按 row / block 切割平行。
   - **VP8 intra（中風險）**：MB 有 left / top 重建相依，無法單純 raster 平行；採 **wavefront（對角線）** 平行或 bounded-lookahead 的 row group。須保持 partition-0 token 的 raster 輸出順序，平行只做運算、序列化只做輸出。

3. **降低配置壓力**
   - 以 `sync.Pool` 重用 per-MB scratch（coefficient blocks、transform 暫存陣列）。
   - VP8L 避免 `transform.go` 的整圖 `deltas` / `blocks` copy，改 in-place 或 buffer reuse。
   - 以第 1 點的 `ReportAllocs` 數字驗證每步降幅。

4. **演算法微調（可攜、低風險）**
   - `applyFilter`（`transform.go:175`）的 float64 Manhattan 距離改為整數運算。
   - mode search 快取 prediction 輸出，避免重複計算；適用處以 SAD 取代 SSE。
   - 收緊 LZ77 比對內迴圈（`writer.go:556` 起）。

5. **明確記錄 tradeoff**
   - 依約束**不採用 `.s` 組語 SIMD**。雖然 libwebp 以 SSE2/AVX2/NEON 在 raw kernel 上有最大加速，但會犧牲可攜性與單一實作的可維護性，與本專案定位不符。本規劃以「並行 + 降配置 + 演算法微調」取得多核與低 GC 壓力的實務收益。

---

## Part 2 — 安全

### Findings（現況）

- **解碼委派 `x/image/webp`**：本專案無原生 VP8/VP8L decoder（`reader.go` 為薄包裝），故**不受 CVE-2023-4863 影響**（該漏洞位於 libwebp 的 VP8L lossless Huffman 解碼）。處理不可信輸入的風險落在成熟、受審視的 `x/image`。
- **無 `unsafe`、無組語**；唯一相依 `golang.org/x/image`（`go.mod`）。
- **維度上限已檢查**：VP8L ≤ 16384（`writer.go:402-407`）、VP8 ≤ 16383（`MaxDimension`，`internal/vp8enc/frame.go:27,44-48`）。
- **`writer.go:626` 距離索引**：經稽核**安全**。`distances` 為 8×16=128 項（`writer.go:564-573`）；該分支守衛為 `x > width-8 && y < 7`（x 靠近 *width*，故 `width-x ∈ [1,7]`），索引範圍 [25,127] < 128，**不會越界**——並非初步掃描誤判的 OOB。仍建議以 fuzz 守住這類手寫索引運算（尤其 width < 8 的邊界）。
- **panic 安全**：`bitWriter.writeBits`（`bitwriter.go:16-23`）對非法 bit 數／過大值 `panic`。設計上是 programmer-error guard，但若病態輸入觸發編碼器邏輯邊界，會以 panic（DoS）而非 error 呈現給呼叫端。
- **32-bit 平台溢位**：`GOARCH=386/arm` 的 `int` 為 32-bit，大圖的 `w*h*…` 尺寸計算需顯式防溢位。
- **Fuzz 覆蓋薄**：僅 `boolcoder_test.go:242`（`FuzzBoolCoderRoundtrip`）；`Encode` / `EncodeAll` 全路徑無 fuzz。
- **無資源／時間上限**：16384² 影像可耗約 1GB 記憶體與大量 CPU，無 budget 或取消機制 → 資源型 DoS。

### Plan（依優先序）

1. **全編碼路徑 fuzz**
   - 新增 `FuzzEncode`：隨機維度、像素內容與多種 `image.Image` 型別（NRGBA / RGBA / Gray / Paletted / YCbCr）。
   - 新增 `FuzzEncodeAll`：隨機動畫參數（frame 數、durations、disposals、loop count）。
   - 用 Go 原生 fuzzing，目標是抓出 panic / OOB / overflow；發現的 corpus 納入 `testdata`。

2. **panic 邊界**
   - 在公開 `Encode` / `EncodeAll` 加 top-level `recover()`，將非預期 panic 轉為 error 回傳，避免函式庫使用者的伺服器被單張圖片打掛。
   - 或者：以稽核 + fuzz 證明 `bitWriter.writeBits`（`bitwriter.go:16-23`）的 panic 在任何合法輸入下都不可能被觸發，並把它降級為 internal invariant 註解。

3. **資源 / DoS 限制**
   - 提供可設定的 `MaxDimension` 與 max total pixel budget（預設保守值），超過即回 error。
   - `context.Context` 取消長時間 encode（與 Part 1 並行、[`plan.md`](./plan.md) Phase B 的 API 擴充共用同一機制）。

4. **32-bit 安全**
   - 對 `w*h`、`w*h*4`、YUV stride 等尺寸計算加入 overflow-safe 檢查（先比較再相乘，或以 64-bit 中介後檢查上界），確保在 32-bit 平台也安全拒絕過大輸入。

5. **供應鏈 / CI**
   - CI 加 `govulncheck`；保持 `golang.org/x/image` 更新——目前 decode 的安全姿態等同 `x/image` 的姿態。
   - 既有 CI 已有 staticcheck，維持。

6. **未來原生 decoder 警示**
   - 若 [`plan.md`](./plan.md) 未來採納原生 decoder，它將成為**不可信輸入的主攻擊面**，必須配備大量 fuzz 與嚴格 bounds checking（CVE-2023-4863 類風險）。在引入前，本文件先標記此前提。

---

## 交叉引用

- **`context.Context` 取消**：同時服務於 Part 1（並行的 worker 控制）與 Part 2 第 3 點（DoS 緩解），並對應 [`plan.md`](./plan.md) Phase B。
- **原生 decoder**：列為 [`plan.md`](./plan.md) 的非目標，同時是本文件 Part 2 第 6 點的安全警示——兩份文件互相對照。
