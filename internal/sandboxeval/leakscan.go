package sandboxeval

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// LeakOutcome 是泄漏扫描的闭合结论：clean 表示未命中，leak_detected 表示在
// 允许扫描的持久化表面发现 Key 字节序列。
type LeakOutcome string

// 泄漏结论的两个闭合取值；任何其他形态都不得写入审计。
const (
	LeakClean    LeakOutcome = "clean"
	LeakDetected LeakOutcome = "leak_detected"
)

// leakScanPathThresholdPerFile 限制单文件读取上限之外的消费方式：文件按整
// 体读入（评测状态目录的 journal/制品均远小于该阈值），超出视作异常并失败
// 关闭，防止把未知大文件的半截内容当作 clean 证据。
const leakScanMaxFileBytes = 1 << 30

// LeakScanReport 汇总一次泄漏扫描：只有闭合结论与逐表面命中计数——绝不
// 记录 Key 本身或其摘要，报告可安全持久化到本地评测状态与审计行。
type LeakScanReport struct {
	Outcome LeakOutcome    `json:"outcome"`
	Hits    map[string]int `json:"hits,omitempty"`
}

// ScanPersistenceSurfaces 用内存中的原始 Key 扫描允许的持久化表面。调用时
// 机由可信进程控制：启动前、每个 Run 后与评测闭合前各一次。任一指定表面
// 无法读取即整体失败关闭——无法证明干净的表面不得视为干净。
func ScanPersistenceSurfaces(paths []string, secret []byte) (LeakScanReport, error) {
	if len(secret) == 0 {
		return LeakScanReport{}, fmt.Errorf("leak scan requires the in-memory secret")
	}
	report := LeakScanReport{Outcome: LeakClean, Hits: map[string]int{}}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return LeakScanReport{}, fmt.Errorf("open surface %q for scan: %w", redactPath(path), err)
		}
		content, err := io.ReadAll(io.LimitReader(file, leakScanMaxFileBytes))
		closeErr := file.Close()
		if err != nil {
			return LeakScanReport{}, fmt.Errorf("read surface %q: %w", redactPath(path), err)
		}
		if closeErr != nil {
			return LeakScanReport{}, fmt.Errorf("close surface %q: %w", redactPath(path), closeErr)
		}
		hits := bytes.Count(content, secret)
		if hits > 0 {
			report.Outcome = LeakDetected
			report.Hits[redactPath(path)] = hits
		}
	}
	return report, nil
}

// redactPath 把扫描表面的路径收敛为最后一段名称：命中计数需要可定位的表
// 面标识，但完整绝对路径属于本机细节，不进入可持久化的报告结构。
func redactPath(path string) string {
	return path[strings.LastIndexByte(path, '/')+1:]
}
