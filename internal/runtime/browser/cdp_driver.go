// cdp_driver.go 实现 Chrome DevTools Protocol 的最小只读驱动：
// 连接已有调试端口或拉起 headless Chrome，执行导航、求值与截图。
// 失败关闭，不开放任意 CDP 方法给模型。
// Author: fxbin
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// CDPTimeout 是单次 CDP 往返的默认超时。
const CDPTimeout = 15 * time.Second

// Driver 是受控浏览器驱动合同。
type Driver interface {
	Navigate(ctx context.Context, url string) error
	Evaluate(ctx context.Context, expression string) (string, error)
	Screenshot(ctx context.Context) ([]byte, error)
	Close() error
}

// ChromeCDPDriver 通过 DevTools 协议控制单个 Chrome 目标。
type ChromeCDPDriver struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	ws        *websocket.Conn
	nextID    int
	targetID  string
	sessionID string
	baseURL   string
}

// CDPOptions 控制 Chrome 启动与连接。
type CDPOptions struct {
	// DebuggingURL 是已有 DevTools 端点，例如 http://127.0.0.1:9222；为空则拉起 Chrome。
	DebuggingURL string
	// ChromePath 是浏览器二进制；空则尝试常见名称。
	ChromePath string
	// UserDataDir 为拉起实例指定临时配置目录。
	UserDataDir string
	// Headless 默认 true。
	Headless bool
}

// LaunchChrome 启动或连接 Chrome，返回可用 Driver。
func LaunchChrome(ctx context.Context, options CDPOptions) (*ChromeCDPDriver, error) {
	driver := &ChromeCDPDriver{}
	if strings.TrimSpace(options.DebuggingURL) != "" {
		driver.baseURL = strings.TrimRight(options.DebuggingURL, "/")
		if err := driver.attach(ctx); err != nil {
			return nil, err
		}
		return driver, nil
	}
	binary := options.ChromePath
	if binary == "" {
		binary = findChromeBinary()
	}
	if binary == "" {
		return nil, errors.New("chrome binary not found; set DebuggingURL or ChromePath")
	}
	userData := options.UserDataDir
	if userData == "" {
		temp, err := os.MkdirTemp("", "cairn-chrome-")
		if err != nil {
			return nil, err
		}
		userData = temp
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + userData,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
	}
	if options.Headless {
		args = append(args, "--headless=new", "--disable-gpu")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	driver.cmd = cmd
	driver.baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
	if err := waitForDevTools(ctx, driver.baseURL); err != nil {
		_ = driver.Close()
		return nil, err
	}
	if err := driver.attach(ctx); err != nil {
		_ = driver.Close()
		return nil, err
	}
	return driver, nil
}

// attach 连接首个 page 目标并开启会话。
func (driver *ChromeCDPDriver) attach(ctx context.Context) error {
	targets, err := driver.listHTTPTargets(ctx)
	if err != nil {
		return err
	}
	pageURL := ""
	for _, target := range targets {
		if target.Type == "page" {
			pageURL = target.WebSocketDebuggerURL
			driver.targetID = target.ID
			break
		}
	}
	if pageURL == "" {
		create, err := driver.httpJSON(ctx, http.MethodGet, "/json/new?about:blank", nil)
		if err != nil {
			return err
		}
		var created struct {
			ID                   string `json:"id"`
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if err := json.Unmarshal(create, &created); err != nil {
			return err
		}
		pageURL = created.WebSocketDebuggerURL
		driver.targetID = created.ID
	}
	if pageURL == "" {
		return errors.New("no page target available")
	}
	conn, _, err := websocket.Dial(ctx, pageURL, nil)
	if err != nil {
		return fmt.Errorf("dial CDP websocket: %w", err)
	}
	driver.ws = conn
	// Target.attachToTarget 便于后续 session 命令；失败时退回无 session。
	raw, err := driver.call(ctx, "Target.attachToTarget", map[string]any{"targetId": driver.targetID, "flatten": true})
	if err == nil {
		var attached struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &attached) == nil {
			driver.sessionID = attached.SessionID
		}
	}
	return nil
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// listHTTPTargets 读取 DevTools 目标列表。
func (driver *ChromeCDPDriver) listHTTPTargets(ctx context.Context) ([]cdpTarget, error) {
	raw, err := driver.httpJSON(ctx, http.MethodGet, "/json/list", nil)
	if err != nil {
		return nil, err
	}
	var targets []cdpTarget
	if err := json.Unmarshal(raw, &targets); err != nil {
		return nil, err
	}
	return targets, nil
}

// httpJSON 调用 DevTools HTTP 端点。
func (driver *ChromeCDPDriver) httpJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, driver.baseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("devtools HTTP %s: status %d", path, response.StatusCode)
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := response.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

// call 发送一次 CDP 命令并等待同 id 响应。
func (driver *ChromeCDPDriver) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.ws == nil {
		return nil, errors.New("cdp websocket is not connected")
	}
	driver.nextID++
	id := driver.nextID
	payload := map[string]any{"id": id, "method": method, "params": params}
	if driver.sessionID != "" {
		payload["sessionId"] = driver.sessionID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, CDPTimeout)
	defer cancel()
	if err := driver.ws.Write(callCtx, websocket.MessageText, encoded); err != nil {
		return nil, err
	}
	for {
		_, raw, err := driver.ws.Read(callCtx)
		if err != nil {
			return nil, err
		}
		var message struct {
			ID    int `json:"id"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			continue
		}
		if message.ID != id {
			continue
		}
		if message.Error != nil {
			return nil, errors.New(message.Error.Message)
		}
		return message.Result, nil
	}
}

// Navigate 导航到显式 URL。
func (driver *ChromeCDPDriver) Navigate(ctx context.Context, url string) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("url is required")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "about:") {
		return errors.New("url must be http(s) or about:")
	}
	_, err := driver.call(ctx, "Page.enable", map[string]any{})
	if err != nil {
		return err
	}
	result, err := driver.call(ctx, "Page.navigate", map[string]any{"url": url})
	if err != nil {
		return err
	}
	var navigation struct {
		ErrorText string `json:"errorText"`
	}
	_ = json.Unmarshal(result, &navigation)
	if navigation.ErrorText != "" {
		return errors.New(navigation.ErrorText)
	}
	return nil
}

// Evaluate 在页面上下文求值并返回 JSON 文本结果。
func (driver *ChromeCDPDriver) Evaluate(ctx context.Context, expression string) (string, error) {
	if strings.TrimSpace(expression) == "" {
		return "", errors.New("expression is required")
	}
	result, err := driver.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return "", err
	}
	var evaluated struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(result, &evaluated); err != nil {
		return "", err
	}
	if evaluated.ExceptionDetails != nil {
		return "", errors.New(evaluated.ExceptionDetails.Text)
	}
	if len(evaluated.Result.Value) == 0 {
		return "null", nil
	}
	return string(evaluated.Result.Value), nil
}

// Screenshot 捕获当前视口 PNG。
func (driver *ChromeCDPDriver) Screenshot(ctx context.Context) ([]byte, error) {
	result, err := driver.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return nil, err
	}
	var captured struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(result, &captured); err != nil {
		return nil, err
	}
	if captured.Data == "" {
		return nil, errors.New("empty screenshot payload")
	}
	return decodeBase64(captured.Data)
}

// Close 回收 WebSocket 与本进程启动的 Chrome。
func (driver *ChromeCDPDriver) Close() error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	var first error
	if driver.ws != nil {
		if err := driver.ws.Close(websocket.StatusNormalClosure, "done"); err != nil && first == nil {
			first = err
		}
		driver.ws = nil
	}
	if driver.cmd != nil && driver.cmd.Process != nil {
		if err := driver.cmd.Process.Kill(); err != nil && first == nil {
			first = err
		}
		_, _ = driver.cmd.Process.Wait()
		driver.cmd = nil
	}
	return first
}

// findChromeBinary 尝试常见 Chrome/Chromium 名称。
func findChromeBinary() string {
	candidates := []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, candidate := range candidates {
		if strings.Contains(candidate, string(filepath.Separator)) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

// waitForDevTools 轮询 DevTools 端点直至可用。
func waitForDevTools(ctx context.Context, baseURL string) error {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/json/version", nil)
		if err == nil {
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode < 300 {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return errors.New("devtools endpoint did not become ready")
}

// freePort 选择空闲 TCP 端口。
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("unexpected listener address type")
	}
	return address.Port, nil
}

// decodeBase64 解码 CDP base64 载荷。
func decodeBase64(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(value)
}
