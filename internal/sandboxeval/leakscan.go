// 本文件实现持久化表面的凭据泄漏扫描。
//
// Author: fxbin
package sandboxeval

import (
	"bytes"
	"errors"
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

// leakScanMaxFileBytes 限制单文件流式扫描上限；超出即失败关闭，防止把未知
// 大文件的半截内容当作 clean 证据。
const leakScanMaxFileBytes = 1 << 30

// leakScanChunkBytes 是流式扫描的固定内存窗口，不随文件大小增长。
const leakScanChunkBytes = 64 * 1024

// LeakScanReport 汇总一次泄漏扫描：只有闭合结论与逐表面命中计数——绝不
// 记录 Key 本身或其摘要，报告可安全持久化到本地评测状态与审计行。
type LeakScanReport struct {
	Outcome LeakOutcome    `json:"outcome"`
	Hits    map[string]int `json:"hits,omitempty"`
}

// ScanPersistenceSurfaces 用内存中的原始 Key 扫描允许的持久化表面。调用时
// 机由可信进程控制：启动前、每个 Run 后与评测闭合前各一次。任一指定表面
// 无法读取或超过扫描体积上限即整体失败关闭——无法证明干净的表面不得视为
// 干净，截断读到的半截内容不得冒充 clean 证据。
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
		hits, err := countSecretStream(io.LimitReader(file, leakScanMaxFileBytes+1), secret)
		closeErr := file.Close()
		if closeErr != nil {
			return LeakScanReport{}, fmt.Errorf("close surface %q: %w", redactPath(path), closeErr)
		}
		if errors.Is(err, errLeakScanSurfaceTooLarge) {
			return LeakScanReport{}, fmt.Errorf("surface %q exceeds the leak scan size contract", redactPath(path))
		}
		if err != nil {
			return LeakScanReport{}, fmt.Errorf("read surface %q: %w", redactPath(path), err)
		}
		if hits > 0 {
			report.Outcome = LeakDetected
			report.Hits[redactPath(path)] = hits
		}
	}
	return report, nil
}

var errLeakScanSurfaceTooLarge = errors.New("leak scan surface exceeds maximum bytes")

// countSecretStream 在固定块窗口内统计秘密字节串，保留至多 len(secret)-1
// 字节跨块重叠；已经在上一块完整计数的命中不会重复累计。
func countSecretStream(reader io.Reader, secret []byte) (int, error) {
	buffer := make([]byte, leakScanChunkBytes)
	carry := make([]byte, 0, len(secret)-1)
	totalBytes := int64(0)
	hits := 0
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			totalBytes += int64(count)
			if totalBytes > leakScanMaxFileBytes {
				return 0, errLeakScanSurfaceTooLarge
			}
			carryLength := len(carry)
			window := make([]byte, 0, carryLength+count)
			window = append(window, carry...)
			window = append(window, buffer[:count]...)
			for offset := 0; offset <= len(window)-len(secret); {
				match := bytes.Index(window[offset:], secret)
				if match < 0 {
					break
				}
				start := offset + match
				if start+len(secret) > carryLength {
					hits++
				}
				offset = start + len(secret)
			}
			keep := len(secret) - 1
			if keep > len(window) {
				keep = len(window)
			}
			carry = append(carry[:0], window[len(window)-keep:]...)
		}
		if readErr == io.EOF {
			return hits, nil
		}
		if readErr != nil {
			return 0, readErr
		}
	}
}

// redactPath 把扫描表面的路径收敛为最后一段名称：命中计数需要可定位的表
// 面标识，但完整绝对路径属于本机细节，不进入可持久化的报告结构。
func redactPath(path string) string {
	return path[strings.LastIndexByte(path, '/')+1:]
}
