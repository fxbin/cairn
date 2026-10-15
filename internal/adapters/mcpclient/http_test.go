package mcpclient

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fxbin/cairn/internal/config"
)

func TestBearerStaysAtConfiguredOriginAndRedirectsDoNotPropagate(t *testing.T) {
	t.Setenv("CAIRN_MCP_TEST_BEARER", "offline-bearer")
	var sourceHits, destinationHits atomic.Int32
	var sourceAuthorization, destinationAuthorization atomic.Value
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		destinationHits.Add(1)
		destinationAuthorization.Store(request.Header.Get("Authorization"))
		_, _ = io.WriteString(writer, "ok")
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sourceHits.Add(1)
		sourceAuthorization.Store(request.Header.Get("Authorization"))
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, destination.URL, http.StatusFound)
			return
		}
		_, _ = io.WriteString(writer, "ok")
	}))
	defer source.Close()
	client, err := newHTTPClient(config.MCPServerConfig{URL: source.URL, BearerTokenEnv: "CAIRN_MCP_TEST_BEARER"}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	for _, endpoint := range []string{source.URL, destination.URL} {
		response, err := client.Get(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if sourceAuthorization.Load() != "Bearer offline-bearer" || destinationAuthorization.Load() != "" {
		t.Fatal("Bearer 未保持原点隔离")
	}
	response, err := client.Get(source.URL + "/redirect")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || sourceHits.Load() != 2 || destinationHits.Load() != 1 {
		t.Fatal("重定向导致额外网络请求或 Bearer 传播")
	}
	t.Setenv("CAIRN_MCP_TEST_BEARER", "")
	if _, err := client.Get(source.URL); err == nil || sourceHits.Load() != 2 {
		t.Fatal("缺失 Bearer 时未在发送前失败")
	}
}

func TestHTTPResponseLimitCountsDecodedBytesAndRejectsTruncation(t *testing.T) {
	for _, mode := range []string{"exact", "declared", "chunked", "compressed"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if mode == "exact" {
					_, _ = io.WriteString(writer, strings.Repeat("x", 32))
					return
				}
				if mode == "compressed" {
					writer.Header().Set("Content-Encoding", "gzip")
					compressor := gzip.NewWriter(writer)
					_, _ = io.WriteString(compressor, strings.Repeat("x", 256))
					_ = compressor.Close()
					return
				}
				if mode == "chunked" {
					writer.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(writer, strings.Repeat("x", 33))
			}))
			defer server.Close()
			client, err := newHTTPClient(config.MCPServerConfig{URL: server.URL}, 32)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			response, err := client.Get(server.URL)
			var body []byte
			if err == nil {
				body, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			if mode == "exact" {
				if err != nil || len(body) != 32 {
					t.Fatalf("刚好达到上限的响应被拒绝：%d %v", len(body), err)
				}
			} else if err == nil || len(body) > 32 {
				t.Fatalf("超限响应被当作完整结果：%d %v", len(body), err)
			}
		})
	}
}
