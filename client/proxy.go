package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

const maxErrorBodyLogBytes int64 = 2048

// aiDomains maps AI API hostnames to their vendor short name.
// 场景说明：Cursor（*.cursor.sh）；GitHub Copilot（VS Code / Visual Studio 等）；Claude Code（api.anthropic.com 等）；
// OpenCode 上游通常直连各厂商 API（本仓库已列 OpenAI/Anthropic/Google/Bedrock 等），本地 opencode serve 默认 127.0.0.1 为直连、不经 MITM。
var aiDomains = map[string]string{
	// ── OpenAI ──
	"api.openai.com": "openai",
	// Codex CLI（ChatGPT Plus/Pro/Team 登录态）默认基址：
	//   https://chatgpt.com/backend-api/codex/responses
	// 走 OAuth 而非 API key，必须把 chatgpt.com 列入 MITM 名单，否则
	// 全部 ChatGPT 计费的 Codex 调用都不会被记录。
	"chatgpt.com":     "openai-codex",
	"www.chatgpt.com": "openai-codex",

	// ── ChatGPT Web (网页版，无标准 usage 字段，走体积估算) ──
	"ab.chatgpt.com":  "chatgpt",
	"chat.openai.com": "chatgpt",

	// ── GitHub Copilot (VS Code, Visual Studio, Kiro 等) ──
	"copilot-proxy.githubusercontent.com": "github-copilot",
	"api.githubcopilot.com":               "github-copilot",
	"api.individual.githubcopilot.com":    "github-copilot",
	"api.business.githubcopilot.com":      "github-copilot",
	"api.enterprise.githubcopilot.com":    "github-copilot",

	// ── Kiro（官方核心服务：chat / code assistance）──
	"runtime.us-east-1.kiro.dev":    "kiro",
	"runtime.eu-central-1.kiro.dev": "kiro",

	// ── GitHub Models ──
	"models.inference.ai.azure.com": "github-models",

	// ── Cursor（IDE 及遥测；通配见 aiWildcardDomains *.cursor.sh）──
	"api2.cursor.sh":    "cursor",
	"api3.cursor.sh":    "cursor",
	"api.cursor.com":    "cursor",
	"metrics.cursor.sh": "cursor",

	// ── Anthropic / Claude Code（CLI 默认 api.anthropic.com；Bedrock 见通配）──
	"api.anthropic.com":     "anthropic",
	"claude.ai":             "anthropic", // Claude Code OAuth 认证
	"platform.claude.com":   "anthropic", // Console 账号认证
	"www.claude.ai":         "anthropic",
	"console.anthropic.com": "anthropic", // Console 管理界面

	// ── Google ──
	"generativelanguage.googleapis.com": "google",
	"aiplatform.googleapis.com":         "google-vertex",

	// ── Mistral / Codestral ──
	"api.mistral.ai":       "mistral",
	"codestral.mistral.ai": "mistral",

	// ── Cohere ──
	"api.cohere.com": "cohere",

	// ── xAI (Grok) ──
	"api.x.ai": "xai",

	// ── Perplexity ──
	"api.perplexity.ai": "perplexity",

	// ── Together ──
	"api.together.xyz": "together",
	"api.together.ai":  "together",

	// ── Groq ──
	"api.groq.com": "groq",

	// ── Replicate ──
	"api.replicate.com": "replicate",

	// ── Hugging Face ──
	"api-inference.huggingface.co": "huggingface",
	"router.huggingface.co":        "huggingface",

	// ── Fireworks ──
	"api.fireworks.ai": "fireworks",

	// ── OpenRouter（Continue、LibreChat、自建客户端等常用聚合端点）──
	"openrouter.ai":     "openrouter",
	"api.openrouter.ai": "openrouter",

	// ── Tabnine ──
	"api.tabnine.com": "tabnine",

	// ── Codeium / Windsurf Cascade（文档中 Cascade 与 Codeium 共用 server.codeium.com）──
	"api.codeium.com":    "codeium",
	"server.codeium.com": "codeium",

	// ── JetBrains AI Assistant ──
	"api.jetbrains.ai":     "jetbrains-ai",
	"llm.api.jetbrains.ai": "jetbrains-ai",

	// ── Sourcegraph Cody Gateway ──
	"cody-gateway.sourcegraph.com": "sourcegraph-cody",

	// ── Augment Code ──
	"api.augmentcode.com":     "augment",
	"dialapi.augmentcode.com": "augment",

	// ── DeepInfra（多模型推理平台，Cline/Aider/Continue 等工具常用）──
	"api.deepinfra.com": "deepinfra",

	// ── Cerebras（高速推理，开发工具集成逐渐增多）──
	"api.cerebras.ai": "cerebras",

	// ── SambaNova Cloud ──
	"api.sambanova.ai": "sambanova",

	// ── Novita AI ──
	"api.novita.ai": "novita",

	// ── fal.ai（部分工作流 / 插件）──
	"fal.run":    "fal",
	"api.fal.ai": "fal",

	// ── China ──
	"api.deepseek.com":            "deepseek",
	"api.moonshot.cn":             "moonshot",
	"open.bigmodel.cn":            "zhipu",
	"aip.baidubce.com":            "baidu",
	"api.minimax.chat":            "minimax",
	"dashscope.aliyuncs.com":      "qwen",
	"dashscope-intl.aliyuncs.com": "qwen",
	// 阿里 qwen-code（gemini-cli fork）OAuth 模式默认走
	//   https://portal.qwen.ai/v1/chat/completions
	// 而不是 dashscope；不补这两个域名，OAuth 用户全部漏报。
	"portal.qwen.ai":              "qwen",
	"chat.qwen.ai":                "qwen",
	"api.lingyiwanwu.com":         "yi",
	"ark.cn-beijing.volces.com":   "doubao",
	"api.baichuan-ai.com":         "baichuan",
	"hunyuan.tencentcloudapi.com": "hunyuan",
	"spark-api-open.xf-yun.com":   "spark",
	"api.sensenova.cn":            "sensetime",
	"api.stepfun.com":             "stepfun",
	"api.tiangong.cn":             "skywork",
	"api.siliconflow.cn":          "siliconflow",

	// 阿里 Qoder（AI IDE，OpenAI / Anthropic 协议混用）
	// 客户端日志里观察到的请求 host：api2.qoder.sh / center.qoder.sh；
	// 不补会一直走 CONNECT 透传 → 0 token 上报。
	"api2.qoder.sh":   "qoder",
	"api3.qoder.sh":   "qoder",
	"api.qoder.sh":    "qoder",
	"center.qoder.sh": "qoder",
	"api.qoder.com":   "qoder",

	// ── OFox AI（OpenAI 兼容接口）──
	"api.ofox.ai": "ofox",
}

// aiWildcardDomains matches AI hostnames by suffix (and optional prefix).
// Checked when exact aiDomains lookup misses.
var aiWildcardDomains = []struct {
	suffix, prefix, vendor string
}{
	// Azure OpenAI: *.openai.azure.com
	{suffix: ".openai.azure.com", vendor: "azure"},
	// AWS Bedrock: bedrock-runtime.*.amazonaws.com
	{suffix: ".amazonaws.com", prefix: "bedrock-runtime.", vendor: "aws-bedrock"},
	{suffix: ".amazonaws.com", prefix: "bedrock.", vendor: "aws-bedrock"},
	// Google Vertex AI: *-aiplatform.googleapis.com
	{suffix: "-aiplatform.googleapis.com", vendor: "google-vertex"},
	// AWS SageMaker: *.sagemaker.aws
	{suffix: ".sagemaker.aws", vendor: "aws-sagemaker"},
	// Amazon CodeWhisperer：codewhisperer.<region>.amazonaws.com
	{suffix: ".amazonaws.com", prefix: "codewhisperer.", vendor: "aws-codewhisperer"},
	// Amazon Q Developer API：q.<region>.amazonaws.com
	{suffix: ".amazonaws.com", prefix: "q.", vendor: "aws-q"},
	// Kiro GovCloud / Amazon Q FIPS endpoint：q-fips.<region>.amazonaws.com
	{suffix: ".amazonaws.com", prefix: "q-fips.", vendor: "aws-q"},
	// Azure AI Inference / Foundry 等：*.inference.azure.com
	{suffix: ".inference.azure.com", vendor: "azure-inference"},
	// Cursor 新增子域（如未来 api*.cursor.sh）
	{suffix: ".cursor.sh", vendor: "cursor"},
	// GitHub Copilot 新增子域
	{suffix: ".githubcopilot.com", vendor: "github-copilot"},
	// 阿里 Qwen 后续子域（如 *.qwen.ai 的新登录入口）
	{suffix: ".qwen.ai", vendor: "qwen"},
	// 阿里 Qoder 新增子域（api*.qoder.sh / center.qoder.sh / *.qoder.com）
	{suffix: ".qoder.sh", vendor: "qoder"},
	{suffix: ".qoder.com", vendor: "qoder"},
}

// pinnedTLSHosts 是已知会做证书钉扎（cert pinning）的主机后缀。
// 这些客户端不信任我们的本地 CA，干脆不 MITM，直接 CONNECT 透传。
// 代价是这些厂商的 token 用量无法记录——但 Cursor 本来走 gRPC/Protobuf也解不出 usage，
// 与其让 IDE 手握失败、用户根本用不了对话，不如透传。
// 注：cursor 系条目可被 cfg.MitmCursor=true 覆盖（仅在确认本机 Cursor 不再 pin 时启用）。
var pinnedTLSHosts = []string{
	".cursor.sh",  // Cursor IDE 桌面端钉证书
	".cursor.com", // 预留：Cursor 主站 / 后续 API 子域
}

// isCursorHost 判断主机是否属于 Cursor 系。
func isCursorHost(hostname string) bool {
	hostname = strings.ToLower(hostname)
	return strings.HasSuffix(hostname, ".cursor.sh") || strings.HasSuffix(hostname, ".cursor.com")
}

// isPinnedTLSHost 判断主机是否在钉扎名单中。cfg 不为 nil 且开启 MitmCursor 时，
// cursor 系主机会从名单中豁免，进入正常 MITM 流程。
func isPinnedTLSHost(hostname string, cfg *Config) bool {
	hostname = strings.ToLower(hostname)
	if cfg != nil && cfg.EffectiveMitmCursor() && isCursorHost(hostname) {
		return false
	}
	for _, suf := range pinnedTLSHosts {
		if strings.HasSuffix(hostname, suf) {
			return true
		}
	}
	return false
}

// quietTunnelHostSuffixes 列出已知的遥测/崩溃上报/分析平台，CONNECT 透传时不打印日志，
// 避免控制台被 OS/IDE/插件的后台心跳刷屏（与 token 监控无关）。
var quietTunnelHostSuffixes = []string{
	".events.data.microsoft.com",
	".in.applicationinsights.azure.com",
	"dc.services.visualstudio.com",
	".visualstudio.com",
	"otel.gitkraken.com",
	".gitkraken.com",
	".posthog.com",
	".sentry.io",
	".segment.io",
	".amplitude.com",
	".datadoghq.com",
	".newrelic.com",
	".bugsnag.com",
	".rollbar.com",
	"vortex.data.microsoft.com",
	"settings-win.data.microsoft.com",
	"mobile.events.data.microsoft.com",
	".aria.microsoft.com",
}

func isQuietTunnelHost(hostname string) bool {
	hostname = strings.ToLower(hostname)
	for _, suf := range quietTunnelHostSuffixes {
		if hostname == strings.TrimPrefix(suf, ".") || strings.HasSuffix(hostname, suf) {
			return true
		}
	}
	return false
}

// matchAIDomain 判断主机名是否应走 MITM，并返回供应商标签（内置表 + config 扩展）。
func (s *ProxyServer) matchAIDomain(hostname string) (string, bool) {
	hostname = normalizeProxyHostname(hostname) // ToLower + TrimSpace + 去除末尾点号
	if vendor, ok := effectiveMonitorHosts(s.cfg)[hostname]; ok {
		vendor = strings.TrimSpace(vendor)
		if vendor != "" {
			return vendor, true
		}
	}
	for _, w := range effectiveMonitorSuffixes(s.cfg) {
		suffix := strings.TrimSpace(strings.ToLower(w.Suffix))
		prefix := strings.TrimSpace(strings.ToLower(w.Prefix))
		vendor := strings.TrimSpace(w.Vendor)
		if suffix == "" || vendor == "" {
			continue
		}
		if strings.HasSuffix(hostname, suffix) {
			if prefix == "" || strings.HasPrefix(hostname, prefix) {
				return vendor, true
			}
		}
	}
	return "", false
}

func normalizeProxyHostname(hostname string) string {
	hostname = strings.TrimSpace(strings.ToLower(hostname))
	hostname = strings.TrimSuffix(hostname, ".")
	return hostname
}

// legacyRoutes maps vendor short names to upstream API base URLs.
var legacyRoutes = map[string]string{
	"openai":      "https://api.openai.com",
	"anthropic":   "https://api.anthropic.com",
	"google":      "https://generativelanguage.googleapis.com",
	"azure":       "",
	"mistral":     "https://api.mistral.ai",
	"cohere":      "https://api.cohere.com",
	"xai":         "https://api.x.ai",
	"perplexity":  "https://api.perplexity.ai",
	"together":    "https://api.together.xyz",
	"groq":        "https://api.groq.com",
	"amazon":      "",
	"deepseek":    "https://api.deepseek.com",
	"moonshot":    "https://api.moonshot.cn",
	"zhipu":       "https://open.bigmodel.cn",
	"baidu":       "https://aip.baidubce.com",
	"minimax":     "https://api.minimax.chat",
	"qwen":        "https://dashscope.aliyuncs.com",
	"yi":          "https://api.lingyiwanwu.com",
	"doubao":      "https://ark.cn-beijing.volces.com",
	"baichuan":    "https://api.baichuan-ai.com",
	"hunyuan":     "https://hunyuan.tencentcloudapi.com",
	"spark":       "https://spark-api-open.xf-yun.com",
	"sensetime":   "https://api.sensenova.cn",
	"stepfun":     "https://api.stepfun.com",
	"skywork":     "https://api.tiangong.cn",
	"siliconflow": "https://api.siliconflow.cn",
}

// ProxyServer is a forward proxy with selective MITM for AI domains
// and backward-compatible reverse proxy for /vendor/path routes.
type ProxyServer struct {
	cfg              *Config
	configPath       string // path to config.json (for runtime wizard updates)
	reporter         *Reporter
	certMgr          *CertManager
	transport        *http.Transport
	upstreamProxy    *url.URL  // parsed upstream proxy; nil = direct
	listenPort       int       // actual bound port (set after listen)
	startedAt        time.Time // when process started
	copilotMu        sync.RWMutex
	copilotDiscounts map[string]float64
	// takeoverMode 是本次进程的网络接管模式，由 main.go 在 startMonitorRuntime 之后
	// 通过 SetTakeoverMode 注入；仅用于 /status 诊断展示。允许值："observe" / "session" / "persistent"。
	takeoverMu   sync.RWMutex
	takeoverMode string
	wizardToken  string
	Updater      *Updater // 自动更新（可为 nil，例如单测）
}

// SetTakeoverMode 在 main.go 决定本次接管模式后调用，让 /status 能如实展示。
// 取值："observe" / "session" / "persistent"；其它字符串会被强制改为 "observe"。
func (s *ProxyServer) SetTakeoverMode(mode string) {
	if s == nil {
		return
	}
	switch mode {
	case "observe", "session", "persistent":
	default:
		mode = "observe"
	}
	s.takeoverMu.Lock()
	s.takeoverMode = mode
	s.takeoverMu.Unlock()
}

func (s *ProxyServer) currentTakeoverMode() string {
	if s == nil {
		return "observe"
	}
	s.takeoverMu.RLock()
	defer s.takeoverMu.RUnlock()
	if s.takeoverMode == "" {
		return "observe"
	}
	return s.takeoverMode
}

var (
	upstreamDialTimeout         = 15 * time.Second
	upstreamProxyConnectTimeout = 20 * time.Second
	upstreamTLSHandshakeTimeout = 20 * time.Second
	upstreamBodyIdleTimeout     = 5 * time.Minute
	proxyTunnelIdleTimeout      = 5 * time.Minute
	// 上游在返回任何响应头之前的最大等待时间（HTTP/2：timeout awaiting response headers）。
	// Codex /chatgpt backend 的 responses/compact 可能由服务端长时间计算后才下发头；
	// 过短会导致 MITM forward error，即便随后直连重试已成功。
	upstreamResponseHeaderTimeout    = 15 * time.Minute
	tunnelRetryInitialBackoff        = 500 * time.Millisecond
	upstreamProxyRetryInitialBackoff = 300 * time.Millisecond
)

func NewProxyServer(cfg *Config, reporter *Reporter, certMgr *CertManager, configPath string) *ProxyServer {
	// Auto-detect upstream proxy (config > system proxy > env vars)
	upstreamAddr := detectUpstreamProxy(cfg)
	proxyFunc := func(*http.Request) (*url.URL, error) { return nil, nil }
	var upstreamURL *url.URL
	if upstreamAddr != "" {
		proxyURL, err := url.Parse(upstreamAddr)
		if err != nil {
			log.Printf("[proxy] invalid upstream proxy %q: %v (fall back direct)", upstreamAddr, err)
		} else {
			proxyFunc = http.ProxyURL(proxyURL)
			upstreamURL = proxyURL
			log.Printf("[proxy] upstream proxy: %s", proxyURL.Redacted())
		}
	} else {
		log.Printf("[proxy] no upstream proxy detected, using direct connection")
	}
	// 配置了 sing-box / Clash 等上游时，Transport 不能用裸 http.ProxyURL：否则对本机目标的
	// outbound（含误送入 MITM 的 http://localhost:*/responses），会被再次转发上游而失败，
	// VS Code Codex「stream disconnected」「error sending request for localhost …」即为典型症状。
	proxyFunc = upstreamProxyPreserveLoopback(proxyFunc)
	return &ProxyServer{
		cfg:              cfg,
		configPath:       configPath,
		reporter:         reporter,
		certMgr:          certMgr,
		upstreamProxy:    upstreamURL,
		startedAt:        time.Now(),
		copilotDiscounts: map[string]float64{},
		transport:        buildUpstreamTransport(proxyFunc),
		wizardToken:      newRequestID(),
	}
}

func (s *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}

	// Status page
	if (r.URL.Host == "" || r.URL.Host == r.Host) && (r.URL.Path == "/" || r.URL.Path == "/status") {
		s.statusPage(w, r)
		return
	}

	// 极简健康探针：watchdog / 外部 keepalive 用，纯内存判断，不读注册表、不动配置。
	// 之所以单独拆出来：/status 要返回 networkTakeoverDiagnostics（4 个 Windows
	// 注册表 read），杀软扫描或 Defender RTP 抢锁时这条路径会偶发 3s+ 超时；
	// 而 watchdog 阈值默认 2 次连续失败就 os.Exit(2)——表象就是"窗口自己没了"。
	if (r.URL.Host == "" || r.URL.Host == r.Host) && (r.URL.Path == "/healthz" || r.URL.Path == "/health") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	// Wizard management page (accessible while monitoring is running)
	if (r.URL.Host == "" || r.URL.Host == r.Host) && strings.HasPrefix(r.URL.Path, "/wizard") {
		s.serveWizard(w, r)
		return
	}

	// Legacy reverse proxy: /openai/v1/...
	if r.URL.Host == "" || r.URL.Host == r.Host {
		trimmed := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(trimmed, "/", 2)
		vendor := strings.ToLower(parts[0])
		if targetBase, ok := legacyRoutes[vendor]; ok {
			s.handleLegacy(w, r, vendor, targetBase, parts)
			return
		}
	}

	// Gateway auto-detect: /v1/* routes (no vendor prefix needed)
	if r.URL.Host == "" || r.URL.Host == r.Host {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			s.handleGatewayRoute(w, r)
			return
		}
	}

	// HTTP forward proxy (absolute URL)
	if r.URL.IsAbs() {
		s.handleHTTPForward(w, r)
		return
	}

	http.Error(w, "Bad Request", http.StatusBadRequest)
}

// ── CONNECT handler ───────────────────────────────────────────

func (s *ProxyServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	hostname := normalizeProxyHostname(host[:strings.LastIndex(host, ":")])

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	vendor, isAI := s.matchAIDomain(hostname)
	if isAI && isPinnedTLSHost(hostname, s.cfg) {
		// 证书钉扎主机：不能 MITM，否则客户端会 EOF 断连。改为透传。
		isAI = false
	}
	if isAI && s.certMgr != nil {
		log.Printf("[CONNECT] MITM → %s (%s)", hostname, vendor)
		go safeGo("mitm "+hostname, func() { s.mitmConnection(clientConn, host, hostname, vendor) })
	} else if isAI {
		log.Printf("[CONNECT] tunnel → %s (%s, no cert manager)", hostname, vendor)
		go safeGo("tunnel "+hostname, func() { s.tunnelConnection(clientConn, host, vendor, hostname) })
	} else {
		if vendor != "" {
			log.Printf("[CONNECT] tunnel → %s (钉证书主机，跳过 MITM)", hostname)
		} else if !isQuietTunnelHost(hostname) {
			log.Printf("[CONNECT] tunnel → %s", hostname)
		}
		go safeGo("tunnel "+hostname, func() { s.tunnelConnection(clientConn, host, vendor, hostname) })
	}
}

// safeGo runs fn with panic recovery so a single malformed request can't crash
// the entire proxy process — which, on a user's machine with system proxy /
// PAC pointing at us, would make every HTTPS site fall back to DIRECT and
// leave HTTP_PROXY env vars pointing at a dead port.
func safeGo(label string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[panic] %s: %v", label, r)
		}
	}()
	fn()
}

func (s *ProxyServer) tunnelConnection(clientConn net.Conn, host, vendor, hostname string) {
	defer clientConn.Close()

	var serverConn net.Conn
	var err error

	// 上游代理不通时（如 sing-box 重启中），以退避重试 3 次。
	// 避免一次 TCP 失败就直接放弃整条 MITM 连接。
	const maxRetries = 3
	retryBackoff := tunnelRetryInitialBackoff
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("[tunnel] retry %d/%d dialing %s (last error: %v)", attempt, maxRetries, host, err)
			time.Sleep(retryBackoff)
			retryBackoff *= 2
			// 重试前刷新传输层的空闲连接池，防止复用死连接
			s.transport.CloseIdleConnections()
		}
		if s.upstreamProxy != nil {
			serverConn, err = s.dialViaUpstreamProxy(host)
		} else {
			serverConn, err = net.DialTimeout("tcp", host, upstreamDialTimeout)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		log.Printf("[tunnel] dial %s failed after %d retries: %v", host, maxRetries+1, err)
		return
	}
	defer serverConn.Close()
	done := make(chan wsCopyResult, 2)
	go func() {
		n, copyErr := copyConnWithIdleTimeout(serverConn, clientConn)
		done <- wsCopyResult{direction: "client->server", bytes: n, err: copyErr}
	}()
	go func() {
		n, copyErr := copyConnWithIdleTimeout(clientConn, serverConn)
		done <- wsCopyResult{direction: "server->client", bytes: n, err: copyErr}
	}()
	first := <-done
	second := <-done
	if first.err != nil && !isClosedNetworkError(first.err) && !isTimeoutNetworkError(first.err) {
		log.Printf("[tunnel] copy ended %s %s: bytes=%d err=%v", first.direction, host, first.bytes, first.err)
	}
	if second.err != nil && !isClosedNetworkError(second.err) && !isTimeoutNetworkError(second.err) {
		log.Printf("[tunnel] copy ended %s %s: bytes=%d err=%v", second.direction, host, second.bytes, second.err)
	}
	s.maybeReportTunnelOpaqueUsage(vendor, hostname, first, second)
}

// maybeReportTunnelOpaqueUsage 对无法 MITM 的 AI CONNECT 透传流量做保守体积估算上报。
// 典型场景：Cursor 钉证书，HTTPS 只能隧道透传；若完全不记，这部分永远是 0。
// 注意：这是粗算（非官方计费口径），仅在 report_opaque_traffic=true 时启用。
func (s *ProxyServer) maybeReportTunnelOpaqueUsage(vendor, hostname string, a, b wsCopyResult) {
	if s == nil || s.reporter == nil || s.cfg == nil || !s.cfg.EffectiveReportOpaqueTraffic() {
		return
	}
	vendor = strings.TrimSpace(vendor)
	if vendor == "" {
		return
	}
	// 官方同步活跃时不再上报 cursor 体积估算，避免与精确数据双计。
	if vendor == "cursor" && CursorOfficialSyncActive() {
		return
	}
	var downBytes int64
	if a.direction == "server->client" {
		downBytes += a.bytes
	}
	if b.direction == "server->client" {
		downBytes += b.bytes
	}
	if downBytes < 64 {
		return
	}
	total := int(downBytes / 4)
	if total <= 0 {
		return
	}
	if total > 500000 {
		total = 500000
	}
	// CONNECT 隧道里拿不到请求体，按 30/70 粗分 prompt/completion。
	prompt := total * 30 / 100
	if prompt < 1 {
		prompt = 1
	}
	completion := total - prompt
	if completion < 1 {
		completion = 1
	}
	sourceApp := ""
	if vendor == "cursor" {
		sourceApp = "cursor"
	}
	rec := UsageRecord{
		Vendor:           vendor,
		Model:            opaqueModelLabel(vendor),
		Endpoint:         "connect://" + hostname,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		Source:           opaqueSourceEstimate,
		SourceApp:        sourceApp,
	}
	if vendor == "cursor" {
		rec.SourceKind = "local_estimate"
		rec.Accuracy = "estimated"
		rec.MergeStatus = "unmatched"
	}
	s.reporter.Add(rec)
	log.Printf("[opaque-tunnel] %s %s: estimated total=%d (down_bytes=%d)", vendor, hostname, total, downBytes)
}

// dialViaUpstreamProxy establishes a TCP tunnel through the upstream HTTP proxy
// by sending a CONNECT request. This ensures non-AI HTTPS traffic also chains
// through the user's corporate/VPN proxy.
func (s *ProxyServer) dialViaUpstreamProxy(targetHost string) (net.Conn, error) {
	proxyAddr := s.upstreamProxy.Host
	if !strings.Contains(proxyAddr, ":") {
		port := s.upstreamProxy.Port()
		if port == "" {
			if s.upstreamProxy.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		proxyAddr = net.JoinHostPort(s.upstreamProxy.Hostname(), port)
	}

	// 到上游代理的连接重试 2 次（sing-box 可能在重启中）
	const maxRetries = 2
	var proxyConn net.Conn
	var dialErr error
	retryBackoff := upstreamProxyRetryInitialBackoff
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("[upstream] retry %d/%d connecting to %s (last: %v)", attempt, maxRetries, proxyAddr, dialErr)
			time.Sleep(retryBackoff)
			retryBackoff *= 2
		}
		proxyConn, dialErr = net.DialTimeout("tcp", proxyAddr, upstreamDialTimeout)
		if dialErr == nil {
			break
		}
	}
	if dialErr != nil {
		return nil, fmt.Errorf("connect upstream proxy %s after %d retries: %w", proxyAddr, maxRetries+1, dialErr)
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetHost, targetHost)
	if s.upstreamProxy.User != nil {
		// Basic auth for upstream proxy
		password, _ := s.upstreamProxy.User.Password()
		auth := s.upstreamProxy.User.Username() + ":" + password
		encoded := base64.StdEncoding.EncodeToString([]byte(auth))
		connectReq += "Proxy-Authorization: Basic " + encoded + "\r\n"
	}
	connectReq += "\r\n"

	if err := proxyConn.SetDeadline(time.Now().Add(upstreamProxyConnectTimeout)); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("set CONNECT deadline on upstream: %w", err)
	}
	if _, err := proxyConn.Write([]byte(connectReq)); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("write CONNECT to upstream: %w", err)
	}

	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("read CONNECT response from upstream: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		proxyConn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT returned %d", resp.StatusCode)
	}
	if err := proxyConn.SetDeadline(time.Time{}); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("clear CONNECT deadline on upstream: %w", err)
	}

	return proxyConn, nil
}

// mitmConnection intercepts TLS traffic for AI domains.
func (s *ProxyServer) mitmConnection(clientConn net.Conn, host, hostname, vendor string) {
	defer clientConn.Close()

	cert, err := s.certMgr.GetCert(hostname)
	if err != nil {
		log.Printf("[MITM] cert error %s: %v", hostname, err)
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		// 与上游一致：Copilot / 多数云 API 默认 HTTP/2（ALPN h2）；仅 http/1.1 时客户端无法对话。
		NextProtos: mitmClientALPN(vendor),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("[MITM] handshake error %s: %v", hostname, err)
		return
	}
	defer tlsConn.Close()

	if tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
		s.serveMitmHTTP2(tlsConn, hostname, vendor)
		return
	}

	reader := bufio.NewReader(tlsConn)

	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if isExpectedDisconnectError(err) {
				return
			}
			// Check if client tried HTTP/2
			if reader.Buffered() > 0 {
				peek, _ := reader.Peek(reader.Buffered())
				log.Printf("[MITM] read error %s: %v (buffered=%d, prefix=%q)", hostname, err, len(peek), string(peek[:min(len(peek), 24)]))
			} else {
				log.Printf("[MITM] read error %s: %v", hostname, err)
			}
			return
		}

		req.URL.Scheme = "https"
		req.URL.Host = hostname
		req.RequestURI = ""

		endpoint := req.URL.Path
		monitorUsage := shouldMonitorAIEndpoint(endpoint)
		requestModel, reqPromptBytes := "", 0
		sourceApp := ""
		if monitorUsage {
			requestModel, reqPromptBytes = s.processRequestBody(req)
			sourceApp = inferSourceAppFromHeaders(req.Header)
			req.Header.Del("Accept-Encoding")
		}

		if isWebSocketUpgrade(req) {
			s.handleWebSocketMITM(tlsConn, reader, req, host, hostname, vendor, endpoint, requestModel, sourceApp)
			return
		}

		resp, err := s.roundTripUpstream(req)
		if err != nil {
			if !shouldSuppressForwardError(endpoint, monitorUsage, err) {
				log.Printf("[MITM] forward error %s%s: %v", hostname, endpoint, err)
			}
			errResp := &http.Response{
				StatusCode: http.StatusBadGateway,
				Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
				Header:        http.Header{"Content-Type": {"application/json"}, "Connection": {"close"}},
				Body:          io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":"proxy: %v"}`, err))),
				ContentLength: -1,
			}
			errResp.Write(tlsConn)
			return
		}

		log.Printf("[MITM] %s %s%s → %d", req.Method, hostname, endpoint, resp.StatusCode)

		if resp.StatusCode >= 400 && monitorUsage && !responseEndpointHasNoTokenUsage(endpoint) {
			errBody := peekAndRestoreResponseBody(resp, maxErrorBodyLogBytes)
			log.Printf("[MITM] error response %s%s: status=%d body=%q", hostname, endpoint, resp.StatusCode, string(errBody))
		} else if monitorUsage {
			var buf bytes.Buffer
			resp.Body = &recordingBody{
				ReadCloser: resp.Body,
				buf:        &buf,
				onClose: func(data []byte) {
					s.processResponseData(vendor, endpoint, requestModel, sourceApp, data, reqPromptBytes)
				},
			}
		}

		writeErr := resp.Write(tlsConn)
		_ = resp.Body.Close()
		if writeErr != nil {
			return
		}
	}
}

func mitmClientALPN(vendor string) []string {
	if isChatGPTLikeVendor(vendor) {
		return []string{"http/1.1"}
	}
	return []string{"h2", "http/1.1"}
}

func isChatGPTLikeVendor(vendor string) bool {
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	return vendor == "chatgpt" || vendor == "openai-codex"
}

// serveMitmHTTP2 在已协商 ALPN=h2 的 TLS 连接上处理 HTTP/2 请求（与 GitHub Copilot 等客户端一致）。
func (s *ProxyServer) serveMitmHTTP2(tlsConn *tls.Conn, hostname, vendor string) {
	h2s := &http2.Server{}
	h2s.ServeConn(tlsConn, &http2.ServeConnOpts{
		Context: context.Background(),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.URL.Scheme = "https"
			if r.URL.Host == "" {
				r.URL.Host = hostname
			}
			r.RequestURI = ""

			endpoint := r.URL.Path
			monitorUsage := shouldMonitorAIEndpoint(endpoint)
			requestModel, reqPromptBytes := "", 0
			sourceApp := ""
			if monitorUsage {
				requestModel, reqPromptBytes = s.processRequestBody(r)
				sourceApp = inferSourceAppFromHeaders(r.Header)
				r.Header.Del("Accept-Encoding")
			}

			resp, err := s.roundTripUpstream(r)
			if err != nil {
				if !shouldSuppressForwardError(endpoint, monitorUsage, err) {
					log.Printf("[MITM/h2] forward error %s%s: %v", hostname, endpoint, err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"proxy: %v"}`, err)))
				return
			}

			log.Printf("[MITM/h2] %s %s%s → %d", r.Method, hostname, endpoint, resp.StatusCode)

			if resp.StatusCode >= 400 && monitorUsage && !responseEndpointHasNoTokenUsage(endpoint) {
				errBody := peekAndRestoreResponseBody(resp, maxErrorBodyLogBytes)
				log.Printf("[MITM/h2] error response %s%s: status=%d body=%q", hostname, endpoint, resp.StatusCode, string(errBody))
			} else if monitorUsage {
				var buf bytes.Buffer
				resp.Body = &recordingBody{
					ReadCloser: resp.Body,
					buf:        &buf,
					onClose: func(data []byte) {
						s.processResponseData(vendor, endpoint, requestModel, sourceApp, data, reqPromptBytes)
					},
				}
			}
			defer resp.Body.Close()

			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			if resp.Body != nil {
				if isStreamingResponse(resp.Header) {
					streamCopy(w, resp.Body)
				} else {
					io.Copy(w, resp.Body)
				}
			}
		}),
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	if r == nil {
		return false
	}
	return headerHasToken(r.Header, "Connection", "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

func headerHasToken(h http.Header, key, token string) bool {
	token = strings.ToLower(strings.TrimSpace(token))
	for _, value := range h.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.ToLower(strings.TrimSpace(part)) == token {
				return true
			}
		}
	}
	return false
}

func (s *ProxyServer) handleWebSocketMITM(clientConn net.Conn, clientReader *bufio.Reader, req *http.Request, host, hostname, vendor, endpoint, requestModel, sourceApp string) {
	wsID := fmt.Sprintf("%x", time.Now().UnixNano())
	started := time.Now()
	diag := isCodexWebSocketEndpoint(vendor, endpoint)
	clientBuffered := 0
	if clientReader != nil {
		clientBuffered = clientReader.Buffered()
	}
	if diag {
		log.Printf("[MITM/ws/diag %s] start %s%s client_local=%s client_remote=%s buffered=%d key=%s protocol=%q client_extensions=%q forwarded_extensions=%q version=%q",
			wsID, hostname, endpoint, safeAddr(clientConn.LocalAddr()), safeAddr(clientConn.RemoteAddr()), clientBuffered,
			req.Header.Get("Sec-WebSocket-Key"),
			req.Header.Get("Sec-WebSocket-Protocol"),
			req.Header.Get("Sec-WebSocket-Extensions"),
			"",
			req.Header.Get("Sec-WebSocket-Version"),
		)
	}

	upstreamConn, err := s.dialTLSUpstream(hostname, host)
	if err != nil {
		log.Printf("[MITM/ws] dial %s%s: %v", hostname, endpoint, err)
		writeHTTPErrorToConn(clientConn, http.StatusBadGateway, fmt.Sprintf("proxy websocket dial: %v", err))
		return
	}
	defer upstreamConn.Close()
	if diag {
		log.Printf("[MITM/ws/diag %s] upstream connected local=%s remote=%s", wsID, safeAddr(upstreamConn.LocalAddr()), safeAddr(upstreamConn.RemoteAddr()))
	}

	req.Header.Del("Proxy-Connection")
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Sec-WebSocket-Extensions")
	req.RequestURI = ""
	req.URL.Scheme = "https"
	req.URL.Host = hostname

	if err := req.Write(upstreamConn); err != nil {
		log.Printf("[MITM/ws] write request %s%s: %v", hostname, endpoint, err)
		if diag {
			log.Printf("[MITM/ws/diag %s] write request failed after=%s err=%v", wsID, time.Since(started), err)
		}
		return
	}

	upstreamReader := bufio.NewReader(upstreamConn)
	resp, err := http.ReadResponse(upstreamReader, req)
	if err != nil {
		log.Printf("[MITM/ws] read response %s%s: %v", hostname, endpoint, err)
		if diag {
			log.Printf("[MITM/ws/diag %s] read response failed after=%s err=%v", wsID, time.Since(started), err)
		}
		writeHTTPErrorToConn(clientConn, http.StatusBadGateway, fmt.Sprintf("proxy websocket response: %v", err))
		return
	}
	defer resp.Body.Close()
	if diag {
		log.Printf("[MITM/ws/diag %s] upstream response status=%d accept=%q protocol=%q extensions=%q conn=%q upgrade=%q buffered=%d after=%s",
			wsID,
			resp.StatusCode,
			resp.Header.Get("Sec-WebSocket-Accept"),
			resp.Header.Get("Sec-WebSocket-Protocol"),
			resp.Header.Get("Sec-WebSocket-Extensions"),
			resp.Header.Get("Connection"),
			resp.Header.Get("Upgrade"),
			upstreamReader.Buffered(),
			time.Since(started),
		)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[MITM/ws] %s %s%s → %d body=%q", req.Method, hostname, endpoint, resp.StatusCode, string(peek))
		resp.Body = io.NopCloser(bytes.NewReader(peek))
		resp.ContentLength = int64(len(peek))
		resp.Header.Del("Transfer-Encoding")
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(peek)))
		if err := resp.Write(clientConn); err != nil {
			return
		}
		return
	}

	log.Printf("[MITM/ws] %s %s%s → %d", req.Method, hostname, endpoint, resp.StatusCode)
	if err := writeWebSocketSwitchResponse(clientConn, resp); err != nil {
		if diag {
			log.Printf("[MITM/ws/diag %s] write 101 to client failed after=%s err=%v", wsID, time.Since(started), err)
		}
		return
	}
	if diag {
		log.Printf("[MITM/ws/diag %s] switched after=%s client_buffered=%d upstream_buffered=%d accept=%q protocol=%q extensions=%q",
			wsID,
			time.Since(started),
			clientBuffered,
			upstreamReader.Buffered(),
			resp.Header.Get("Sec-WebSocket-Accept"),
			resp.Header.Get("Sec-WebSocket-Protocol"),
			resp.Header.Get("Sec-WebSocket-Extensions"),
		)
	}

	done := make(chan wsCopyResult, 2)
	streamEstimator := newCopilotResponsesStreamEstimator(
		vendor,
		endpoint,
		requestModel,
		sourceApp,
		s.githubCopilotDiscountMultiplier(requestModel),
		s.reporter,
	)
	go func() {
		src := io.Reader(&idleDeadlineReader{r: clientConn, conn: clientConn, idle: proxyTunnelIdleTimeout})
		if clientReader != nil && clientReader.Buffered() > 0 {
			src = io.MultiReader(clientReader, src)
		}
		n, err := io.Copy(&idleDeadlineWriter{w: upstreamConn, conn: upstreamConn, idle: proxyTunnelIdleTimeout}, src)
		if diag {
			log.Printf("[MITM/ws/diag %s] client->server ended bytes=%d after=%s err=%v closed=%v", wsID, n, time.Since(started), err, isClosedNetworkError(err))
		}
		if err != nil && !isClosedNetworkError(err) && !isTimeoutNetworkError(err) {
			log.Printf("[MITM/ws] client->server copy ended %s%s: %v", hostname, endpoint, err)
		}
		done <- wsCopyResult{direction: "client->server", bytes: n, err: err}
	}()
	go func() {
		serverToClient := &countingWriter{w: &idleDeadlineWriter{w: clientConn, conn: clientConn, idle: proxyTunnelIdleTimeout}}
		frameCount := 0
		serverReader := &idleDeadlineReader{r: upstreamReader, conn: upstreamConn, idle: proxyTunnelIdleTimeout}
		err := copyWebSocketServerToClientObserved(serverToClient, serverReader, func(payload []byte) {
			s.processResponseData(vendor, endpoint, requestModel, sourceApp, payload, 0)
			streamEstimator.Observe(payload)
		}, func(frame websocketFrame, frameErr error) {
			if !diag {
				return
			}
			frameCount++
			if frameCount <= 8 || frameErr != nil {
				log.Printf("[MITM/ws/diag %s] server frame #%d opcode=%d fin=%v masked=%v payload=%d raw=%d err=%v",
					wsID, frameCount, frame.opcode, frame.fin, frame.masked, len(frame.payload), len(frame.raw), frameErr)
			}
		})
		if diag {
			log.Printf("[MITM/ws/diag %s] server->client ended bytes=%d frames=%d after=%s err=%v closed=%v", wsID, serverToClient.n, frameCount, time.Since(started), err, isClosedNetworkError(err))
		}
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF && !isClosedNetworkError(err) && !isTimeoutNetworkError(err) {
			log.Printf("[MITM/ws] server->client copy ended %s%s: %v", hostname, endpoint, err)
		}
		streamEstimator.Flush()
		done <- wsCopyResult{direction: "server->client", bytes: serverToClient.n, err: err}
	}()
	first := <-done
	if diag {
		log.Printf("[MITM/ws/diag %s] first direction ended=%s bytes=%d err=%v total_after=%s closing peers", wsID, first.direction, first.bytes, first.err, time.Since(started))
	}
	// WebSocket 是一条逻辑连接；任一方向结束后必须关闭两端 socket，
	// 否则另一侧 goroutine 可能卡在半关闭连接上，表现为 CloseWait 且 Codex 一直 Working。
	_ = clientConn.Close()
	_ = upstreamConn.Close()
	second := <-done
	if diag {
		log.Printf("[MITM/ws/diag %s] second direction ended=%s bytes=%d err=%v total_after=%s closing tunnel", wsID, second.direction, second.bytes, second.err, time.Since(started))
	}
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "forcibly closed by the remote host") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe")
}

func isExpectedDisconnectError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isClosedNetworkError(err) {
		return true
	}
	if err == nil {
		return false
	}
	// net/http 在 body 被提前消费后再次 RoundTrip 抛出的内部错误，常见于 APM
	// 心跳/批量上报这类 fire-and-forget 流量，对终端用户没有诊断价值。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid read on closed body")
}

func isTimeoutNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type wsCopyResult struct {
	direction string
	bytes     int64
	err       error
}

func isCodexWebSocketEndpoint(vendor, endpoint string) bool {
	return strings.EqualFold(strings.TrimSpace(vendor), "openai-codex") &&
		strings.Contains(strings.ToLower(endpoint), "/backend-api/codex/responses")
}

func safeAddr(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func (s *ProxyServer) dialTLSUpstream(hostname, host string) (*tls.Conn, error) {
	var rawConn net.Conn
	var err error
	if s.upstreamProxy != nil {
		rawConn, err = s.dialViaUpstreamProxy(host)
	} else {
		rawConn, err = net.DialTimeout("tcp", host, upstreamDialTimeout)
	}
	if err != nil {
		return nil, err
	}
	if err := rawConn.SetDeadline(time.Now().Add(upstreamTLSHandshakeTimeout)); err != nil {
		rawConn.Close()
		return nil, err
	}
	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: hostname,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return nil, err
	}
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		tlsConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func writeHTTPErrorToConn(conn net.Conn, status int, msg string) {
	resp := &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"text/plain; charset=utf-8"}, "Connection": {"close"}},
		Body:          io.NopCloser(strings.NewReader(msg)),
		ContentLength: int64(len(msg)),
	}
	_ = resp.Write(conn)
}

func writeWebSocketSwitchResponse(w io.Writer, resp *http.Response) error {
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	if accept := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Accept")); accept != "" {
		b.WriteString("Sec-WebSocket-Accept: ")
		b.WriteString(accept)
		b.WriteString("\r\n")
	}
	if protocol := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Protocol")); protocol != "" {
		b.WriteString("Sec-WebSocket-Protocol: ")
		b.WriteString(protocol)
		b.WriteString("\r\n")
	}
	if extensions := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Extensions")); extensions != "" {
		b.WriteString("Sec-WebSocket-Extensions: ")
		b.WriteString(extensions)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func copyWebSocketServerToClient(dst io.Writer, src io.Reader, onMessage func([]byte)) error {
	return copyWebSocketServerToClientObserved(dst, src, onMessage, nil)
}

func copyWebSocketServerToClientObserved(dst io.Writer, src io.Reader, onMessage func([]byte), onFrame func(websocketFrame, error)) error {
	var acc websocketMessageAccumulator
	for {
		frame, err := readWebSocketFrame(src)
		if onFrame != nil {
			onFrame(frame, err)
		}
		if len(frame.raw) > 0 {
			if _, writeErr := dst.Write(frame.raw); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return err
			}
			if isClosedNetworkError(err) {
				return err
			}
			// Inspection is best-effort only. Keep the WebSocket transparent
			// when we hit an unsupported frame shape or an oversized payload.
			_, copyErr := io.Copy(dst, src)
			if copyErr != nil {
				return fmt.Errorf("%w; raw fallback failed: %v", err, copyErr)
			}
			return err
		}
		acc.observe(frame, onMessage)
	}
}

type websocketFrame struct {
	raw     []byte
	opcode  byte
	fin     bool
	masked  bool
	maskKey [4]byte
	payload []byte
}

func readWebSocketFrame(r io.Reader) (websocketFrame, error) {
	var f websocketFrame
	header := make([]byte, 2)
	if n, err := io.ReadFull(r, header); err != nil {
		f.raw = append(f.raw, header[:n]...)
		return f, err
	}
	f.raw = append(f.raw, header...)
	f.fin = header[0]&0x80 != 0
	f.opcode = header[0] & 0x0f
	f.masked = header[1]&0x80 != 0
	payloadLen := uint64(header[1] & 0x7f)

	switch payloadLen {
	case 126:
		ext := make([]byte, 2)
		if n, err := io.ReadFull(r, ext); err != nil {
			f.raw = append(f.raw, ext[:n]...)
			return f, err
		}
		f.raw = append(f.raw, ext...)
		payloadLen = uint64(ext[0])<<8 | uint64(ext[1])
	case 127:
		ext := make([]byte, 8)
		if n, err := io.ReadFull(r, ext); err != nil {
			f.raw = append(f.raw, ext[:n]...)
			return f, err
		}
		f.raw = append(f.raw, ext...)
		payloadLen = 0
		for _, b := range ext {
			payloadLen = payloadLen<<8 | uint64(b)
		}
	}

	if payloadLen > recordingBodyMaxBytes {
		return f, fmt.Errorf("websocket frame too large: %d bytes", payloadLen)
	}
	if f.masked {
		if n, err := io.ReadFull(r, f.maskKey[:]); err != nil {
			f.raw = append(f.raw, f.maskKey[:n]...)
			return f, err
		}
		f.raw = append(f.raw, f.maskKey[:]...)
	}
	f.payload = make([]byte, int(payloadLen))
	if n, err := io.ReadFull(r, f.payload); err != nil {
		f.payload = f.payload[:n]
		f.raw = append(f.raw, f.payload...)
		return f, err
	}
	f.raw = append(f.raw, f.payload...)
	return f, nil
}

type websocketMessageAccumulator struct {
	active bool
	opcode byte
	buf    bytes.Buffer
}

func (a *websocketMessageAccumulator) observe(frame websocketFrame, onMessage func([]byte)) {
	if onMessage == nil {
		return
	}
	payload := framePayloadForInspect(frame)
	switch frame.opcode {
	case 0x1, 0x2:
		if frame.fin {
			onMessage(payload)
			return
		}
		a.active = true
		a.opcode = frame.opcode
		a.buf.Reset()
		a.write(payload)
	case 0x0:
		if !a.active {
			return
		}
		a.write(payload)
		if frame.fin {
			msg := make([]byte, a.buf.Len())
			copy(msg, a.buf.Bytes())
			a.active = false
			a.buf.Reset()
			onMessage(msg)
		}
	}
}

func (a *websocketMessageAccumulator) write(payload []byte) {
	if len(payload) == 0 {
		return
	}
	if a.buf.Len()+len(payload) > recordingBodyMaxBytes {
		a.active = false
		a.buf.Reset()
		return
	}
	a.buf.Write(payload)
}

func framePayloadForInspect(frame websocketFrame) []byte {
	if !frame.masked {
		return frame.payload
	}
	payload := make([]byte, len(frame.payload))
	for i, b := range frame.payload {
		payload[i] = b ^ frame.maskKey[i%4]
	}
	return payload
}

// ── HTTP forward proxy ────────────────────────────────────────

func (s *ProxyServer) handleHTTPForward(w http.ResponseWriter, r *http.Request) {
	hostname := r.URL.Hostname()
	if isLinkLocalMetadataHost(hostname) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	vendor, isAI := s.matchAIDomain(hostname)
	monitorUsage := isAI && shouldMonitorAIEndpoint(r.URL.Path)

	requestModel := ""
	sourceApp := ""
	if monitorUsage {
		requestModel, _ = s.processRequestBody(r)
		sourceApp = inferSourceAppFromHeaders(r.Header)
		r.Header.Del("Accept-Encoding")
	}

	resp, err := s.roundTripUpstream(r)
	if err != nil {
		if !shouldSuppressForwardError(r.URL.Path, monitorUsage, err) {
			log.Printf("[HTTP] forward error %s %s: %v", r.Method, r.URL.String(), err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("[HTTP] %s %s → %d", r.Method, r.URL.String(), resp.StatusCode)

	if monitorUsage && resp.StatusCode >= 400 {
		errBody := peekAndRestoreResponseBody(resp, maxErrorBodyLogBytes)
		log.Printf("[HTTP] error response %s %s: status=%d body=%q", r.Method, r.URL.String(), resp.StatusCode, string(errBody))
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if monitorUsage && resp.StatusCode < 400 {
		var buf bytes.Buffer
		tee := io.TeeReader(resp.Body, &buf)
		if isStreamingResponse(resp.Header) {
			streamCopy(w, tee)
		} else {
			io.Copy(w, tee)
		}
		go s.processResponseData(vendor, r.URL.Path, requestModel, sourceApp, buf.Bytes(), 0)
	} else {
		if isStreamingResponse(resp.Header) {
			streamCopy(w, resp.Body)
		} else {
			io.Copy(w, resp.Body)
		}
	}
}

// ── Legacy reverse proxy (/vendor/path) ───────────────────────

func (s *ProxyServer) handleLegacy(w http.ResponseWriter, r *http.Request, vendor, targetBase string, parts []string) {
	remaining := "/"
	if len(parts) > 1 {
		remaining = "/" + parts[1]
	}

	if vendor == "azure" || vendor == "amazon" {
		if ep := r.Header.Get("X-Azure-Endpoint"); ep != "" {
			targetBase = ep
			r.Header.Del("X-Azure-Endpoint")
		} else if ep := r.Header.Get("X-Endpoint"); ep != "" {
			targetBase = ep
			r.Header.Del("X-Endpoint")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": vendor + " 需要设置 X-Endpoint 请求头"})
			return
		}
	}

	if targetBase == "" {
		http.Error(w, "unknown vendor", http.StatusBadRequest)
		return
	}

	requestModel, _ := s.processRequestBody(r)
	sourceApp := inferSourceAppFromHeaders(r.Header)

	target, err := url.Parse(targetBase)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = remaining
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = target.Host
			req.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 400 {
				return nil
			}
			// 无论 SSE 还是普通 JSON 响应，统一用 recordingBody 异步采集 usage。
			// 这样既能实时流式下发（FlushInterval:-1），又不会因 Content-Type 不是
			// text/event-stream 而漏记 chunked JSON / gRPC-Web 等格式的响应。
			var buf bytes.Buffer
			resp.Body = &recordingBody{
				ReadCloser: resp.Body, buf: &buf,
				onClose: func(data []byte) { s.processResponseData(vendor, remaining, requestModel, sourceApp, data, 0) },
			}
			return nil
		},
		FlushInterval: -1,
		Transport:     upstreamRoundTripper{server: s},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			s.closeIdleUpstreamConnections()
			if !shouldSuppressForwardError(remaining, shouldMonitorAIEndpoint(remaining), err) {
				log.Printf("[legacy] forward error %s: %v", targetBase+remaining, err)
			}
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		},
	}

	log.Printf("[legacy] %s %s → %s%s", r.Method, vendor, targetBase, remaining)
	proxy.ServeHTTP(w, r)
}

// ── Shared helpers ────────────────────────────────────────────

// shouldInjectOpenAIStreamOptions 判断是否可注入 OpenAI 专有的 stream_options。
// Anthropic Messages API、Copilot→Claude 等请求若带上该字段会 400（Extra inputs are not permitted）。
func shouldInjectOpenAIStreamOptions(r *http.Request, reqData map[string]interface{}) bool {
	// 请求体含 anthropic_version → Anthropic 协议，勿注入
	if _, ok := reqData["anthropic_version"]; ok {
		return false
	}
	host := strings.ToLower(r.URL.Hostname())
	path := strings.ToLower(r.URL.Path)

	// Anthropic 端点（含直连与 gateway 模式下 path=/v1/messages）
	if strings.Contains(host, "anthropic.com") {
		return false
	}
	if strings.HasPrefix(path, "/v1/messages") {
		return false
	}
	// Copilot 网关（含 Claude）；勿注入 OpenAI 专有字段
	if strings.Contains(host, "githubcopilot.com") ||
		strings.Contains(host, "copilot-proxy.githubusercontent.com") {
		return false
	}
	// Codeium / Windsurf Cascade 使用自有协议，不接受 stream_options
	if strings.Contains(host, "codeium.com") {
		return false
	}
	// Google AI / Vertex：OpenAI-compatible 端点可能不接受 stream_options
	if strings.Contains(host, "googleapis.com") || strings.Contains(host, "aiplatform.googleapis.com") {
		return false
	}
	// 明确是 OpenAI 或 Azure OpenAI：直接注入
	if strings.Contains(host, "api.openai.com") || strings.Contains(host, "openai.azure.com") {
		return true
	}
	// 标准 OpenAI Chat Completions / Responses API 路径（多数 OpenAI-compatible 网关）
	if strings.Contains(path, "/v1/chat/completions") || strings.Contains(path, "/v1/responses") {
		return true
	}
	return false
}

func (s *ProxyServer) processRequestBody(r *http.Request) (model string, promptTextBytes int) {
	if r.Body == nil || r.ContentLength == 0 {
		return "", 0
	}
	// 体积上限：超过 16MB 的请求体不再整包读入。
	// 触发场景：Codex /backend-api/codex/responses/compact 把完整会话历史塞进单条请求，
	// 量级可能到几十 MB；之前无上限的 io.ReadAll 会把整个 body 灌进内存，
	// 既让 self-watchdog 探针 3s 内拿不到 /status 把进程踢掉，又在低内存机器上引发 OOM。
	// 超限路径下我们放弃模型识别 / prompt 估算，但流量照常透传，不影响用户使用。
	const maxReqBodyPeek = 16 * 1024 * 1024
	if r.ContentLength > maxReqBodyPeek {
		log.Printf("[proxy] 请求体 %d 字节超过 %d 上限，跳过解析直接透传 (host=%s path=%s)",
			r.ContentLength, maxReqBodyPeek, r.URL.Hostname(), r.URL.Path)
		return "", 0
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxReqBodyPeek+1))
	if err == nil && len(bodyBytes) > maxReqBodyPeek {
		// ContentLength 未知 / chunked 编码下后置兜底：把已读字节 + 剩余流粘回去再透传。
		rest := r.Body
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(bodyBytes), rest), rest}
		r.ContentLength = -1
		r.TransferEncoding = nil
		log.Printf("[proxy] 请求体超过 %d 上限（chunked），跳过解析直接透传 (host=%s path=%s)",
			maxReqBodyPeek, r.URL.Hostname(), r.URL.Path)
		return "", 0
	}
	r.Body.Close()
	if err != nil || len(bodyBytes) == 0 {
		return "", 0
	}

	var reqData map[string]interface{}
	if json.Unmarshal(bodyBytes, &reqData) != nil {
		// gRPC / protobuf 请求：从 proto 帧中提取文本字节作为 prompt tokens 估算基准
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		r.ContentLength = int64(len(bodyBytes))
		r.TransferEncoding = nil // 已完整读取，移除 chunked 标记，避免上游收到两套 framing 信号
		return inferModelHint(bodyBytes), extractGRPCTextBytes(bodyBytes)
	}

	model, _ = reqData["model"].(string)
	if model == "" {
		model = deepFindModel(reqData)
	}

	// 将旧式 thinking.type=enabled+budget_tokens 改写为新式 thinking.type=adaptive+output_config.effort。
	// GitHub Copilot 后端（claude-opus-4-7 等新模型）已不接受旧格式，返回 400 invalid_request_error。
	if rewriteThinkingEnabled(reqData) {
		if modified, err := json.Marshal(reqData); err == nil {
			bodyBytes = modified
			log.Printf("[proxy] rewrite: thinking.type=enabled → thinking.type=adaptive (host=%s)", r.URL.Hostname())
		}
	}

	// 规范化 output_config.effort：VS Code Copilot 扩展会直接发 "xhigh"，
	// 该值不是 Anthropic 合法枚举（合法值：low/medium/high），GitHub Copilot
	// 后端会返回 400 invalid_reasoning_effort；统一降为 "high"。
	if from, to, ok := rewriteOutputConfigEffort(reqData); ok {
		if modified, err := json.Marshal(reqData); err == nil {
			bodyBytes = modified
			log.Printf("[proxy] rewrite: output_config.effort %q → %q (host=%s)", from, to, r.URL.Hostname())
		}
	}

	// Haiku 等非 reasoning 模型不接受 output_config.effort / thinking 字段，
	// 上游会返回 400 invalid_reasoning_effort。剥离这些字段，避免调用方一刀切配置导致请求失败。
	if stripped := stripReasoningForNonReasoningModel(reqData, model); stripped != "" {
		if modified, err := json.Marshal(reqData); err == nil {
			bodyBytes = modified
			log.Printf("[proxy] rewrite: 模型 %s 不支持 reasoning，已剥离 %s (host=%s)", model, stripped, r.URL.Hostname())
		}
	}

	// 仅对 OpenAI Chat Completions 类 API 注入 stream_options（含 include_usage）。
	// Anthropic、GitHub Copilot（含 Claude 后端）等会拒绝未知字段，报 invalid_request_error。
	if stream, ok := reqData["stream"].(bool); ok && stream {
		if _, has := reqData["stream_options"]; !has && shouldInjectOpenAIStreamOptions(r, reqData) {
			reqData["stream_options"] = map[string]interface{}{"include_usage": true}
			if modified, err := json.Marshal(reqData); err == nil {
				bodyBytes = modified
			}
		}
	}

	// 对于 JSON 请求，累加 messages[*].content 的字节数作为 prompt 估算基准。
	// 这给后续 opaque 路径提供更精确的 prompt tokens（当 usage 字段缺失时）。
	if msgs, ok := reqData["messages"].([]interface{}); ok {
		for _, m := range msgs {
			if msg, ok := m.(map[string]interface{}); ok {
				switch c := msg["content"].(type) {
				case string:
					promptTextBytes += len(c)
				case []interface{}:
					// 多模态 content blocks
					for _, block := range c {
						if b, ok := block.(map[string]interface{}); ok {
							if t, ok := b["text"].(string); ok {
								promptTextBytes += len(t)
							}
						}
					}
				}
			}
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	r.ContentLength = int64(len(bodyBytes))
	r.TransferEncoding = nil // 已完整读取，移除 chunked 标记
	if model == "" {
		model = inferModelHint(bodyBytes)
	}
	return model, promptTextBytes
}

// responseEndpointHasNoTokenUsage 为账户/支付/内部服务等响应（无 OpenAI/Anthropic 式 usage 字段）的 API，不打印「extract failed」避免刷屏。
// 无 usage 时后面仍会走 opaque 粗算逻辑（若配置开启且 shouldOpaqueEstimate 为真）。
func responseEndpointHasNoTokenUsage(endpoint string) bool {
	ep := strings.TrimRight(strings.ToLower(endpoint), "/")
	if ep == "" {
		ep = "/"
	}
	if ep == "/backend-api/codex/responses/compact" {
		return true
	}
	// 复用 opaque denylist：这些接口天然无 token usage
	for _, kw := range opaqueEndpointDenylist {
		if strings.Contains(ep, strings.ToLower(kw)) {
			return true
		}
	}
	// 观测/导出（OpenTelemetry OTLP）：无 ChatGPT/OpenAI usage 语义，不配「extract failed」刷屏
	if strings.Contains(ep, "/otlp/") {
		return true
	}
	// 账户/支付接口
	if strings.Contains(ep, "full_stripe_profile") || strings.Contains(ep, "stripe") {
		return true
	}
	if strings.Contains(ep, "dashboardservice") || strings.Contains(ep, "dashboard") {
		return true
	}
	// 模型/Agent 枚举和 session bootstrap 只返回元数据或临时 token，没有用量字段。
	if isNoUsageMetadataEndpoint(ep) {
		return true
	}
	// 认证/账户管理类
	if strings.Contains(ep, "auth") || strings.Contains(ep, "oauth") ||
		strings.Contains(ep, "login") || strings.Contains(ep, "signup") ||
		strings.Contains(ep, "account") || strings.Contains(ep, "billing") ||
		strings.Contains(ep, "subscription") || strings.Contains(ep, "profile") {
		return true
	}
	// 健康检查/状态接口
	if ep == "/health" || ep == "/healthz" || ep == "/ping" || ep == "/status" ||
		strings.HasSuffix(ep, "/health") || strings.HasSuffix(ep, "/healthz") {
		return true
	}
	return false
}

func isNoUsageMetadataEndpoint(ep string) bool {
	if ep == "/agents/sessions" || strings.HasSuffix(ep, "/agents/sessions") || strings.Contains(ep, "/agents/sessions/") {
		return true
	}
	// ChatGPT 插件/商店元数据接口只返回插件清单或权限状态；403 时也常返回整页 HTML，
	// 没有 token usage 语义，不能按模型响应错误打印正文。
	if ep == "/backend-api/plugins" || strings.HasPrefix(ep, "/backend-api/plugins/") ||
		ep == "/backend-api/ps/plugins" || strings.HasPrefix(ep, "/backend-api/ps/plugins/") {
		return true
	}
	if ep == "/models" || ep == "/v1/models" || ep == "/api/v1/models" ||
		ep == "/models/session" || strings.HasSuffix(ep, "/models/session") ||
		ep == "/agents" || strings.HasSuffix(ep, "/agents") {
		return true
	}
	if strings.HasSuffix(ep, "/models") && (strings.Contains(ep, "/agents/") || strings.Contains(ep, "/agent/")) {
		return true
	}
	if strings.Contains(ep, "/agents/swe/") && strings.HasSuffix(ep, "/enabled") {
		return true
	}
	return false
}

func shouldSuppressForwardError(endpoint string, monitorUsage bool, err error) bool {
	if responseEndpointHasNoTokenUsage(endpoint) && isExpectedDisconnectError(err) {
		return true
	}
	return !monitorUsage && responseEndpointHasNoTokenUsage(endpoint) && isExpectedDisconnectError(err)
}

func isLinkLocalMetadataHost(hostname string) bool {
	hostname = normalizeProxyHostname(hostname)
	return hostname == "169.254.169.254"
}

func shouldMonitorAIEndpoint(endpoint string) bool {
	return !responseEndpointHasNoTokenUsage(endpoint)
}

func peekAndRestoreResponseBody(resp *http.Response, maxBytes int64) []byte {
	if resp == nil || resp.Body == nil || maxBytes <= 0 {
		return nil
	}
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if len(peek) == 0 {
		return peek
	}
	originalBody := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(peek), originalBody),
		Closer: originalBody,
	}
	return peek
}

func (s *ProxyServer) processResponseData(vendor, endpoint, requestModel, sourceApp string, data []byte, promptTextBytes int) {
	if vendor == "github-copilot" {
		s.updateGitHubCopilotDiscounts(data)
	}

	usage := ExtractUsage(vendor, data)
	streamEvent := isCopilotResponsesStreamEvent(vendor, endpoint, data)
	if usage == nil {
		if !streamEvent && !responseEndpointHasNoTokenUsage(endpoint) {
			prefix := data
			if len(prefix) > 120 {
				prefix = prefix[:120]
			}
			log.Printf("[usage] %s %s: extract failed, data_len=%d, prefix=%q", vendor, endpoint, len(data), string(prefix))
		}
	} else if usage.TotalTokens == 0 {
		log.Printf("[usage] %s %s: TotalTokens=0 (prompt=%d, completion=%d, model=%q, sourceApp=%q)",
			vendor, endpoint, usage.PromptTokens, usage.CompletionTokens, usage.Model, sourceApp)
	}

	if usage != nil && usage.TotalTokens > 0 {
		model := usage.Model
		if model == "" {
			model = requestModel
		}
		if model == "" {
			model = inferModelHint(data)
		}
		if model == "" {
			model = "unknown"
		}
		log.Printf("[usage] %s %s: reported model=%q prompt=%d completion=%d total=%d sourceApp=%q",
			vendor, endpoint, model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, sourceApp)
		rec := UsageRecord{
			Vendor:              vendor,
			Model:               model,
			Endpoint:            endpoint,
			PromptTokens:        usage.PromptTokens,
			CompletionTokens:    usage.CompletionTokens,
			TotalTokens:         usage.TotalTokens,
			CacheReadTokens:     usage.CacheReadTokens,
			CacheCreationTokens: usage.CacheCreationTokens,
			CostMultiplier:      s.githubCopilotDiscountMultiplier(model),
			Source:              "client",
			SourceApp:           sourceApp,
		}
		if vendor == "cursor" {
			if CursorOfficialSyncActive() {
				// 官方同步活跃 → 这条估算注定与官方精确记录重复，直接丢弃。
				return
			}
			rec.SourceKind = "local_estimate"
			rec.Accuracy = "estimated"
			rec.MergeStatus = "unmatched"
		}
		s.reporter.Add(rec)
		return
	}

	// Codex WebSocket 会持续推送大量非 usage 文本帧。它们不是二进制黑盒流量，
	// 不能按体积估算，否则会和后续 response.completed usage 精确记录重复计数。
	if isCodexWebSocketEndpoint(vendor, endpoint) {
		return
	}

	// gRPC/Protobuf 等：响应体非 JSON 或不含 usage，按配置做体积粗算，使 135 可见（非官方计费）。
	if s.cfg == nil || !s.cfg.EffectiveReportOpaqueTraffic() {
		return
	}
	modelHint := requestModel
	if modelHint == "" {
		modelHint = inferModelHint(data)
	}
	if !shouldOpaqueEstimateForVendor(vendor, endpoint, modelHint, data) {
		if !streamEvent && !responseEndpointHasNoTokenUsage(endpoint) {
			log.Printf("[opaque] skip %s %s: modelHint=%q data_len=%d json_valid=%v",
				vendor, endpoint, modelHint, len(data), json.Valid(data))
		}
		return
	}
	pt, ct, tt := opaqueTokenSplit(data, endpoint, promptTextBytes)
	if tt <= 0 {
		return
	}
	rec := UsageRecord{
		Vendor:           vendor,
		Model:            opaqueModelLabelWithHint(vendor, modelHint),
		Endpoint:         endpoint,
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		Source:           opaqueSourceEstimate,
		SourceApp:        sourceApp,
	}
	if vendor == "cursor" {
		if CursorOfficialSyncActive() {
			return
		}
		rec.SourceKind = "local_estimate"
		rec.Accuracy = "estimated"
		rec.MergeStatus = "unmatched"
	}
	s.reporter.Add(rec)
}

func (s *ProxyServer) statusPage(w http.ResponseWriter, r *http.Request) {
	upstreamLabel := "(direct)"
	if s.upstreamProxy != nil {
		upstreamLabel = s.upstreamProxy.Redacted()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           "running",
		"version":          Version,
		"mode":             "transparent-mitm",
		"network_takeover": s.networkTakeoverDiagnostics(),
		"pid":              os.Getpid(),
		"port":             s.listenPort,
		"wizard_url":       fmt.Sprintf("http://127.0.0.1:%d/wizard", s.listenPort),
		"uptime_seconds":   int(time.Since(s.startedAt).Seconds()),
		"upstream_proxy":   upstreamLabel,
		"user":             s.cfg.UserName,
		"department":       s.cfg.Department,
		"source_app":       s.reporter.sourceApp,
		"server":           s.cfg.ServerURL,
		"monitor_hosts":    len(effectiveMonitorHosts(s.cfg)),
		"monitor_suffixes": len(effectiveMonitorSuffixes(s.cfg)),
		"mitm_cursor":      s.cfg.EffectiveMitmCursor(),
		"stats": map[string]interface{}{
			"total_reported": s.reporter.Stats.TotalReported.Load(),
			"total_tokens":   s.reporter.Stats.TotalTokens.Load(),
		},
	})
}

// networkTakeoverDiagnostics 汇总「ai-monitor 究竟改了哪些系统设置」，
// 用户/排障同事可以直接 GET /status 看清楚而不必去翻注册表。
// 字段：
//
//	mode               -> observe / session / persistent
//	auto_config_url    -> 当前注册表里的 AutoConfigURL（空串表示未设）
//	pac_is_self        -> 当前 PAC 是否指向本程序
//	system_proxy       -> 当前 WinINet 手动代理（空串表示未启用）
//	user_http_proxy    -> HKCU\Environment 中的 HTTP_PROXY（不同于进程级，能反映「持久」设置）
//	user_https_proxy   -> 同上
//	ca_path            -> 本程序生成的 CA 证书路径，便于检查 NODE_EXTRA_CA_CERTS 等
func (s *ProxyServer) networkTakeoverDiagnostics() map[string]interface{} {
	mode := s.currentTakeoverMode()
	autoConfigURL := ReadCurrentAutoConfigURL()
	sysProxy := readCurrentSystemProxy()
	caPath := ""
	if s.certMgr != nil {
		caPath = s.certMgr.CACertPath()
	}
	return map[string]interface{}{
		"mode":                 mode,
		"auto_config_url":      autoConfigURL,
		"pac_is_self":          isAIMonitorPACURL(autoConfigURL),
		"system_proxy":         sysProxy,
		"system_proxy_is_self": isSelfProxy(sysProxy),
		"user_http_proxy":      ReadUserLevelEnv("HTTP_PROXY"),
		"user_https_proxy":     ReadUserLevelEnv("HTTPS_PROXY"),
		"user_no_proxy":        ReadUserLevelEnv("NO_PROXY"),
		"ca_path":              caPath,
	}
}

// recordingBody wraps an io.ReadCloser, recording all bytes read.
// 为防止超长流式响应（如 Claude Opus 的 reasoning）撑爆内存，
// buf 设置上限；超出后只保留尾部，仍能从最后一段提取 usage 信息。
const recordingBodyMaxBytes = 4 * 1024 * 1024 // 4MB

type recordingBody struct {
	io.ReadCloser
	buf       *bytes.Buffer
	onClose   func([]byte)
	dropped   bool
	closeOnce sync.Once
	closeErr  error
}

func (r *recordingBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		if r.buf.Len()+n > recordingBodyMaxBytes {
			// 只保留尾段：丢弃 buf 头部，腾出空间。usage 通常在最后一帧。
			over := r.buf.Len() + n - recordingBodyMaxBytes
			if over >= r.buf.Len() {
				r.buf.Reset()
			} else {
				keep := r.buf.Bytes()[over:]
				cp := make([]byte, len(keep))
				copy(cp, keep)
				r.buf.Reset()
				r.buf.Write(cp)
			}
			r.dropped = true
		}
		r.buf.Write(p[:n])
	}
	return n, err
}

func (r *recordingBody) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.ReadCloser.Close()
		if r.onClose != nil {
			data := make([]byte, r.buf.Len())
			copy(data, r.buf.Bytes())
			go safeGo("recordingBody.onClose", func() { r.onClose(data) })
		}
	})
	return r.closeErr
}

// streamCopy 将 src 拷到 dst，适用于 SSE / chunked 等需要实时下发的响应。
// 每读到一个 chunk 立刻 Flush，否则 Copilot/Claude 的流式输出会被缓冲到连接关闭
// 才统一下发，表现为「回复为空 / 永远转圈」。
func streamCopy(dst http.ResponseWriter, src io.Reader) {
	flusher, _ := dst.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

func (s *ProxyServer) roundTripUpstream(req *http.Request) (*http.Response, error) {
	if s == nil || s.transport == nil {
		return nil, errors.New("upstream transport not configured")
	}
	ctx, cancel := context.WithCancel(req.Context())
	resp, err := s.transport.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		s.closeIdleUpstreamConnections()
		return nil, err
	}
	if resp.Body != nil {
		resp.Body = &idleTimeoutReadCloser{
			ReadCloser: resp.Body,
			cancel:     cancel,
			idle:       upstreamBodyIdle(req, resp.Header),
			label:      req.URL.String(),
		}
	} else {
		cancel()
	}
	return resp, nil
}

// upstreamBodyIdleForReq 控制从上游读取响应体时的「连续无字节」超时（见 idleTimeoutReadCloser）。
//
// ChatGPT/Codex Responses API（/backend-api/codex/responses…）可能出现极长时间静默：
// remote compact、长推理、SSE/分块下发前的首包等待等；若沿用默认 5 分钟「无字节即取消」，会
// cancel 请求 context 并关闭 body，Codex 常见报错：stream disconnected before completion。
// 对上述路径禁用 idle timer，交由上游或服务端正常结束或由客户端断开。
func upstreamBodyIdleForReq(req *http.Request) time.Duration {
	if req == nil || req.URL == nil {
		return upstreamBodyIdleTimeout
	}
	p := strings.ToLower(req.URL.Path)
	if strings.Contains(p, "/backend-api/codex/responses") {
		return 0
	}
	return upstreamBodyIdleTimeout
}

// upstreamBodyIdle 在 upstreamBodyIdleForReq（基于请求路径的特例）之上，
// 进一步根据「响应是否为流式」决定响应体的「连续无字节」超时。
//
// 任何 SSE / chunked / 无 Content-Length 的流式响应（OpenAI、Anthropic
// /v1/messages、GitHub Copilot / Cursor 的 /chat/completions、各类 reasoning
// 模型等）在模型长时间推理时，响应体可能长时间无字节下发；若沿用默认 5 分钟
// 「无字节即取消」，会 cancel 请求 context 并关闭 body，客户端表现为
// "stream disconnected before completion"。对流式响应禁用 idle timer，
// 交由上游/服务端正常结束或由客户端断开。
//
// 非流式响应（带 Content-Length 的整块 JSON）仍保留 idle timer，用于
// 检测真正卡死的上游连接，避免连接句柄长期泄漏。
func upstreamBodyIdle(req *http.Request, respHeader http.Header) time.Duration {
	if isStreamingResponse(respHeader) {
		return 0
	}
	return upstreamBodyIdleForReq(req)
}

func (s *ProxyServer) closeIdleUpstreamConnections() {
	if s != nil && s.transport != nil {
		s.transport.CloseIdleConnections()
	}
}

type upstreamRoundTripper struct {
	server *ProxyServer
}

func (t upstreamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.server.roundTripUpstream(req)
}

type idleTimeoutReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration
	label  string

	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

func (r *idleTimeoutReadCloser) Read(p []byte) (int, error) {
	r.arm()
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.arm()
	}
	if err != nil {
		r.stopTimer()
	}
	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	cancel := r.cancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return r.ReadCloser.Close()
}

func (r *idleTimeoutReadCloser) arm() {
	if r.idle <= 0 || r.cancel == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.timer == nil {
		r.timer = time.AfterFunc(r.idle, func() {
			if r.label != "" {
				log.Printf("[proxy] upstream body idle timeout after %s: %s", r.idle, r.label)
			}
			r.cancel()
		})
		return
	}
	r.timer.Reset(r.idle)
}

func (r *idleTimeoutReadCloser) stopTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
}

type idleDeadlineReader struct {
	r    io.Reader
	conn interface{ SetReadDeadline(time.Time) error }
	idle time.Duration
}

func (r *idleDeadlineReader) Read(p []byte) (int, error) {
	if r.idle > 0 && r.conn != nil {
		_ = r.conn.SetReadDeadline(time.Now().Add(r.idle))
	}
	n, err := r.r.Read(p)
	if err != nil && r.conn != nil {
		_ = r.conn.SetReadDeadline(time.Time{})
	}
	return n, err
}

type idleDeadlineWriter struct {
	w    io.Writer
	conn interface{ SetWriteDeadline(time.Time) error }
	idle time.Duration
}

func (w *idleDeadlineWriter) Write(p []byte) (int, error) {
	if w.idle > 0 && w.conn != nil {
		_ = w.conn.SetWriteDeadline(time.Now().Add(w.idle))
	}
	n, err := w.w.Write(p)
	if err != nil && w.conn != nil {
		_ = w.conn.SetWriteDeadline(time.Time{})
	}
	return n, err
}

func copyConnWithIdleTimeout(dst, src net.Conn) (int64, error) {
	return io.Copy(
		&idleDeadlineWriter{w: dst, conn: dst, idle: proxyTunnelIdleTimeout},
		&idleDeadlineReader{r: src, conn: src, idle: proxyTunnelIdleTimeout},
	)
}

// isStreamingResponse 判断响应是否属于需要即时下发的类型。
func isStreamingResponse(h http.Header) bool {
	ct := h.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return true
	}
	// chunked 且不是二进制/json整体响应：也采取实时下发。
	if h.Get("Transfer-Encoding") == "chunked" || h.Get("Content-Length") == "" {
		return true
	}
	return false
}

// rewriteThinkingEnabled 将旧式 thinking 格式转换为新式格式，返回是否做了改写。
//
// 旧格式（claude-3-7-sonnet 时代）：
//
//	{"thinking": {"type": "enabled", "budget_tokens": 10000}}
//
// 新格式（claude-opus-4-7 等新模型要求）：
//
//	{"thinking": {"type": "adaptive"}, "output_config": {"effort": "high"}}
//
// GitHub Copilot 后端已升级为仅接受新格式，旧格式会收到 400:
// "thinking.type.enabled is not supported for this model."
func rewriteThinkingEnabled(reqData map[string]interface{}) bool {
	thinking, ok := reqData["thinking"].(map[string]interface{})
	if !ok {
		return false
	}
	if t, _ := thinking["type"].(string); t != "enabled" {
		return false
	}
	// budget_tokens → effort 映射（参考 Anthropic 文档建议值）
	budget := toInt(thinking["budget_tokens"])
	effort := "high"
	if budget > 0 && budget <= 2048 {
		effort = "low"
	} else if budget <= 8192 {
		effort = "medium"
	}
	reqData["thinking"] = map[string]interface{}{"type": "adaptive"}
	if _, has := reqData["output_config"]; !has {
		reqData["output_config"] = map[string]interface{}{"effort": effort}
	}
	return true
}

// modelSupportsReasoning 粗判模型是否支持 reasoning（即 output_config.effort / thinking 字段）。
// Anthropic 系：opus 4.x / sonnet 4.x 支持；haiku 全系列 + sonnet 3.x 及以前不支持。
// 命中"不支持"返回 false；未知模型按"支持"放行，避免误伤。
func modelSupportsReasoning(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return true
	}
	if strings.Contains(m, "haiku") {
		return false
	}
	// claude-3*-sonnet / claude-sonnet-3* 等 3.x 不支持 reasoning
	if strings.Contains(m, "claude-3") || strings.Contains(m, "sonnet-3") {
		return false
	}
	return true
}

// stripReasoningForNonReasoningModel 对不支持 reasoning 的模型剥离 output_config.effort 与 thinking 字段。
// 返回被剥离字段的描述（空字符串表示未改写）。
func stripReasoningForNonReasoningModel(reqData map[string]interface{}, model string) string {
	if modelSupportsReasoning(model) {
		return ""
	}
	var stripped []string
	if outCfg, ok := reqData["output_config"].(map[string]interface{}); ok {
		if _, has := outCfg["effort"]; has {
			delete(outCfg, "effort")
			stripped = append(stripped, "output_config.effort")
			if len(outCfg) == 0 {
				delete(reqData, "output_config")
			}
		}
	}
	if _, ok := reqData["thinking"]; ok {
		delete(reqData, "thinking")
		stripped = append(stripped, "thinking")
	}
	if _, ok := reqData["reasoning_effort"]; ok {
		delete(reqData, "reasoning_effort")
		stripped = append(stripped, "reasoning_effort")
	}
	if len(stripped) == 0 {
		return ""
	}
	return strings.Join(stripped, ", ")
}

// rewriteOutputConfigEffort 规范化 output_config.effort 字段。
// VS Code Copilot 等客户端会发送非标准值（如 "xhigh"），GitHub Copilot 后端
// 仅接受 Anthropic 合法枚举：low / medium / high。
// 返回 (原值, 新值, 是否做了改写)。
func rewriteOutputConfigEffort(reqData map[string]interface{}) (string, string, bool) {
	outCfg, ok := reqData["output_config"].(map[string]interface{})
	if !ok {
		return "", "", false
	}
	effort, ok := outCfg["effort"].(string)
	if !ok {
		return "", "", false
	}
	switch effort {
	case "low", "medium", "high":
		return effort, effort, false // 合法值，无需改写
	case "xhigh":
		outCfg["effort"] = "high"
		return effort, "high", true
	default:
		// 其他未知值统一降为 medium，避免 400
		outCfg["effort"] = "medium"
		return effort, "medium", true
	}
}

// upstreamProxyPreserveLoopback 包装 Transport.Proxy：环回与链路本地地址永远不走上游代理。
// 上游 http.ProxyURL 会对所有 host 返回代理地址；若某请求（例如被误配置为走系统代理的
// HTTP 客户端）将 http://localhost:PORT 送到本 MITM，则二次 RoundTrip 仍会走 sing-box，
// 常被上游丢弃或阻断，导致 Codex 扩展本机 /responses 桥接流中断。
func upstreamProxyPreserveLoopback(base func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if shouldDialUpstreamTargetDirect(req) {
			return nil, nil
		}
		if base == nil {
			return nil, nil
		}
		return base(req)
	}
}

func shouldDialUpstreamTargetDirect(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.TrimSpace(req.URL.Hostname())
	if host == "" {
		return false
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	switch h {
	case "localhost", "localhost.localdomain", "::1":
		return true
	}
	ip := net.ParseIP(h)
	if ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return true
		}
		return ip.Equal(net.IPv4(169, 254, 169, 254))
	}
	return strings.HasSuffix(h, ".localhost")
}

// buildUpstreamTransport 创建向上游 AI API 发请求的 Transport。
// 核心：设置了 TLSClientConfig 后 Go 不再自动注册 HTTP/2，需显式调用
// http2.ConfigureTransport，否则每条 MITM 连接都要单独完成一次 TLS 握手，
// 并发量稍高就会出现 "TLS handshake timeout"。
//
// 连接池配置说明（与上游 sing-box/Clash 等本地代理配合）：
//   - IdleConnTimeout 设为 30s，必须短于上游代理的空闲超时，避免持有已
//     被上游关闭的"半死连接"导致请求卡住；
//   - MaxConnsPerHost 限制对每个 AI API 域名的总连接数，防止耗尽上游代理
//     的文件描述符或连接限制；
//   - ResponseHeaderTimeout：避免永远等不到头；但 Codex compact 等可能 >1min，见 upstreamResponseHeaderTimeout。
func buildUpstreamTransport(proxyFunc func(*http.Request) (*url.URL, error)) *http.Transport {
	t := &http.Transport{
		// 默认直连；仅在检测到 upstream_proxy 时走上游代理，避免环路。
		Proxy:                 proxyFunc,
		TLSHandshakeTimeout:   20 * time.Second,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       50,               // 防止耗尽上游代理连接数
		IdleConnTimeout:       30 * time.Second, // 短于 sing-box/Clash 的默认空闲超时
		ResponseHeaderTimeout: upstreamResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   upstreamDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	// 显式启用 HTTP/2，使代理能与上游（如 GitHub Copilot、Anthropic）做 H2 多路复用，
	// 大幅减少并发 TLS 握手次数，消除高并发下的握手超时。
	if err := http2.ConfigureTransport(t); err != nil {
		log.Printf("[proxy] http2.ConfigureTransport: %v (falling back to HTTP/1.1 upstream)", err)
	}
	return t
}
