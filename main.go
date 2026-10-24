package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	addr      = ":8788"
	uploadDir = "uploads"
	maxBytes  = 2 << 30 // 2 GiB
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Fatalf("创建上传目录失败: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", serveIndex)
	mux.HandleFunc("POST /upload", handleUpload)

	handler := withRequestLog(mux)

	logStartupURLs(addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}

func logStartupURLs(addr string) {
	port := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		port = addr[i:]
	}
	log.Printf("服务已启动，打开以下地址上传文件")
	log.Printf("  本机: http://localhost%s", port)
	lans := lanIPv4s()
	if len(lans) == 0 {
		log.Printf("  局域网: 未发现可用地址")
		return
	}
	for _, ip := range lans {
		log.Printf("  局域网: http://%s%s", ip, port)
	}
}

func lanIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			s := ip.String()
			if seen[s] {
				continue
			}
			seen[s] = true
			ips = append(ips, s)
		}
	}
	return ips
}

func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		start := time.Now()
		log.Printf("请求 %s %s ip=%s ua=%q content-length=%s",
			r.Method, r.URL.RequestURI(), ip, r.UserAgent(), formatBytes(r.ContentLength))

		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		log.Printf("响应 %s %s ip=%s status=%d 耗时=%s",
			r.Method, r.URL.RequestURI(), ip, rw.status, time.Since(start).Truncate(time.Millisecond))
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "static/index.html")
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	total := r.ContentLength
	log.Printf("任务开始 上传 ip=%s content-length=%s", ip, formatBytes(total))

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	progress := newProgressReader(r.Body, ip, total)
	r.Body = progress

	reader, err := r.MultipartReader()
	if err != nil {
		log.Printf("任务失败 上传 ip=%s err=不是有效的 multipart 请求", ip)
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "不是有效的 multipart 请求"})
		return
	}

	var saved string
	var written int64
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("任务失败 上传 ip=%s err=读取上传内容失败: %v", ip, err)
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取上传内容失败"})
			return
		}

		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}

		name := sanitizeFilename(part.FileName())
		if name == "" {
			_ = part.Close()
			log.Printf("任务失败 上传 ip=%s err=未选择文件", ip)
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请选择文件"})
			return
		}

		progress.setName(name)
		log.Printf("任务进行 上传 ip=%s file=%s", ip, name)

		dst, tmpPath, err := createUniqueTmp(uploadDir, name)
		if err != nil {
			_ = part.Close()
			log.Printf("任务失败 上传 ip=%s file=%s err=无法保存文件: %v", ip, name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "无法保存文件"})
			return
		}

		written, err = io.Copy(dst, part)
		_ = dst.Close()
		_ = part.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
			log.Printf("任务失败 上传 ip=%s file=%s err=写入文件失败: %v", ip, name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "写入文件失败"})
			return
		}

		finalPath, err := commitUpload(tmpPath, uploadDir, name)
		if err != nil {
			_ = os.Remove(tmpPath)
			log.Printf("任务失败 上传 ip=%s file=%s err=完成文件失败: %v", ip, name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "无法保存文件"})
			return
		}
		saved = filepath.Base(finalPath)
		break
	}

	progress.flush(true)
	if saved == "" {
		log.Printf("任务失败 上传 ip=%s err=未收到文件", ip)
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "未收到文件"})
		return
	}

	log.Printf("任务完成 上传 ip=%s file=%s 已收=%s 文件=%s",
		ip, saved, formatBytes(progress.n), formatBytes(written))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "filename": saved})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

type progressReader struct {
	io.ReadCloser
	mu      sync.Mutex
	ip      string
	name    string
	n       int64
	total   int64
	lastN   int64
	lastAt  time.Time
	lastPct int
}

func newProgressReader(body io.ReadCloser, ip string, total int64) *progressReader {
	return &progressReader{
		ReadCloser: body,
		ip:         ip,
		total:      total,
	}
}

func (p *progressReader) setName(name string) {
	p.mu.Lock()
	p.name = name
	p.mu.Unlock()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if n > 0 {
		p.mu.Lock()
		p.n += int64(n)
		p.mu.Unlock()
		p.flush(false)
	}
	if err == io.EOF {
		p.flush(true)
	}
	return n, err
}

func (p *progressReader) flush(force bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pct := -1
	if p.total > 0 {
		pct = int(p.n * 100 / p.total)
		if pct > 100 {
			pct = 100
		}
	}

	if p.n == p.lastN && pct == p.lastPct && !p.lastAt.IsZero() {
		return
	}
	now := time.Now()
	if !force && !p.lastAt.IsZero() && now.Sub(p.lastAt) < 300*time.Millisecond && pct/5 == p.lastPct/5 {
		return
	}
	p.lastAt = now
	p.lastPct = pct
	p.lastN = p.n

	name := p.name
	if name == "" {
		name = "(解析中)"
	}
	if pct >= 0 {
		log.Printf("上传进度 ip=%s file=%s %s/%s %d%%",
			p.ip, name, formatBytes(p.n), formatBytes(p.total), pct)
		return
	}
	log.Printf("上传进度 ip=%s file=%s 已收=%s", p.ip, name, formatBytes(p.n))
}

func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "." || name == ".." || name == "" {
		return ""
	}

	var b strings.Builder
	for _, r := range name {
		if r == 0 || r == '/' || r == '\\' || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

const tmpAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomSuffix(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = tmpAlphabet[int(b)%len(tmpAlphabet)]
	}
	return string(out), nil
}

func createUniqueTmp(dir, name string) (*os.File, string, error) {
	for range 100 {
		suf, err := randomSuffix(6)
		if err != nil {
			return nil, "", err
		}
		path := filepath.Join(dir, name+"_"+suf)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, path, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("无法创建临时文件")
}

func commitUpload(tmpPath, dir, name string) (string, error) {
	try := func(dest string) (bool, error) {
		if err := os.Link(tmpPath, dest); err == nil {
			if rmErr := os.Remove(tmpPath); rmErr != nil {
				log.Printf("清理临时文件失败 path=%s err=%v", tmpPath, rmErr)
			}
			return true, nil
		} else if os.IsExist(err) {
			return false, nil
		} else {
			return false, err
		}
	}

	dest := filepath.Join(dir, name)
	ok, err := try(dest)
	if err != nil {
		return "", err
	}
	if ok {
		return dest, nil
	}

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		dest = filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		ok, err := try(dest)
		if err != nil {
			return "", err
		}
		if ok {
			return dest, nil
		}
	}
}

func formatBytes(n int64) string {
	if n < 0 {
		return "未知"
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB"}
	val := float64(n)
	unit := "B"
	for i := 0; i < len(units) && val >= 1024; i++ {
		val /= 1024
		unit = units[i]
	}
	return fmt.Sprintf("%.1f %s", val, unit)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
