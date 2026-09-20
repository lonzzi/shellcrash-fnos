package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const coreControllerAddress = "127.0.0.1:9999"

type managedProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	mu       sync.Mutex
	err      error
	finished bool
}

func (a *manager) runNative() error {
	if a.coreDataDir == "" || a.gatewaySocket == "" || a.runtimeConfigPath == "" || a.dashboardDir == "" {
		return errors.New("native service paths are incomplete")
	}
	host, _, err := net.SplitHostPort(a.controllerAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("Mihomo controller must bind to a loopback address")
	}
	if info, err := os.Stat(filepath.Join(a.dashboardDir, "index.html")); err != nil || !info.Mode().IsRegular() {
		return errors.New("MetaCubeXD dashboard files are missing")
	}
	if err := a.prepareNativeRuntime(); err != nil {
		return err
	}
	if err := a.validateCoreConfig(); err != nil {
		return err
	}

	core, err := a.startCore()
	if err != nil {
		return err
	}
	defer core.stop(10 * time.Second)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 45*time.Second)
	err = a.waitForCore(readyCtx, core)
	readyCancel()
	if err != nil {
		return err
	}

	managerListener, err := net.Listen("tcp", a.listenAddr)
	if err != nil {
		return fmt.Errorf("manager listener unavailable: %w", err)
	}
	defer managerListener.Close()

	if err := os.MkdirAll(filepath.Dir(a.gatewaySocket), 0o755); err != nil {
		return fmt.Errorf("gateway socket directory unavailable: %w", err)
	}
	if info, statErr := os.Lstat(a.gatewaySocket); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("gateway socket path exists and is not a socket")
		}
		if err := os.Remove(a.gatewaySocket); err != nil {
			return fmt.Errorf("stale gateway socket cannot be removed: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("gateway socket path cannot be inspected: %w", statErr)
	}
	gatewayListener, err := net.Listen("unix", a.gatewaySocket)
	if err != nil {
		return fmt.Errorf("gateway socket unavailable: %w", err)
	}
	if err := os.Chmod(a.gatewaySocket, 0o666); err != nil {
		gatewayListener.Close()
		return fmt.Errorf("gateway socket permissions cannot be set: %w", err)
	}
	defer func() {
		gatewayListener.Close()
		_ = os.Remove(a.gatewaySocket)
	}()

	managerMux := http.NewServeMux()
	managerMux.HandleFunc("/api/status", a.handleStatus)
	managerMux.HandleFunc("/api/subscription", a.handleSubscription)
	managerMux.HandleFunc("/api/refresh", a.handleRefresh)
	managerMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	managerMux.HandleFunc("/", a.handleWeb)

	managerServer := &http.Server{
		Handler:           securityHeaders(managerMux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	gatewayServer := &http.Server{
		Handler:           securityHeaders(a.gatewayHandler()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- managerServer.Serve(managerListener) }()
	go func() { serveErrors <- gatewayServer.Serve(gatewayListener) }()
	go a.runScheduler()
	log.Printf("native ShellCrash manager listening on %s; fnOS socket ready", a.listenAddr)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	select {
	case signal := <-signals:
		log.Printf("received %s; stopping Mihomo and fnOS gateway", signal)
	case serveErr := <-serveErrors:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("native HTTP service failed: %w", serveErr)
		}
	case <-core.done:
		return fmt.Errorf("Mihomo Core exited: %w", core.exitError())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = gatewayServer.Shutdown(shutdownCtx)
	_ = managerServer.Shutdown(shutdownCtx)
	return nil
}

func (a *manager) prepareNativeRuntime() error {
	profileData, err := os.ReadFile(a.configPath)
	if err != nil {
		return errors.New("ShellCrash profile is unavailable; existing configuration was left untouched")
	}
	secret, err := os.ReadFile(a.secretFile)
	if err != nil {
		return errors.New("Mihomo API secret is unavailable")
	}
	secretValue := strings.TrimSpace(string(secret))
	if len(secretValue) < 32 || strings.ContainsAny(secretValue, "\r\n") {
		return errors.New("Mihomo API secret is invalid")
	}

	doc, root, err := decodeConfig(profileData)
	if err != nil {
		return errors.New("ShellCrash profile is not valid YAML; existing configuration was left untouched")
	}
	setMappingValue(root, "mixed-port", intNode(a.mixedPort))
	if mappingValue(root, "allow-lan") == nil {
		setMappingValue(root, "allow-lan", boolNode(true))
	}
	if mappingValue(root, "bind-address") == nil {
		setMappingValue(root, "bind-address", stringNode("*"))
	}
	controllerAddr := a.controllerAddr
	if controllerAddr == "" {
		controllerAddr = coreControllerAddress
	}
	setMappingValue(root, "external-controller", stringNode(controllerAddr))
	setMappingValue(root, "secret", stringNode(secretValue))

	profileWithNativeEndpoints, err := encodeConfig(doc)
	if err != nil {
		return errors.New("ShellCrash profile could not be prepared")
	}

	overlayData, err := os.ReadFile(a.overlayPath)
	if errors.Is(err, os.ErrNotExist) {
		overlayData, err = overlayFromSubscription(profileData)
	}
	if err != nil {
		return errors.New("DNS/TUN settings could not be loaded")
	}
	_, overlayRoot, err := decodeOverlay(overlayData)
	if err != nil {
		return err
	}
	_, runtimeRoot, err := decodeConfig(profileWithNativeEndpoints)
	if err != nil {
		return errors.New("prepared ShellCrash profile is invalid")
	}
	if err := mergeMapping(runtimeRoot, overlayRoot); err != nil {
		return errors.New("DNS/TUN settings could not be applied")
	}
	runtimeDoc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{runtimeRoot}}
	runtimeData, err := encodeConfig(runtimeDoc)
	if err != nil {
		return errors.New("runtime profile could not be generated")
	}
	if err := os.MkdirAll(filepath.Dir(a.runtimeConfigPath), 0o700); err != nil {
		return errors.New("runtime configuration directory could not be created")
	}
	if err := atomicWrite(a.runtimeConfigPath, runtimeData, 0o600); err != nil {
		return errors.New("runtime configuration could not be saved")
	}
	return nil
}

func intNode(value int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", value)}
}

func (a *manager) validateCoreConfig() error {
	if a.coreBinary == "" {
		return errors.New("Mihomo binary is not configured")
	}
	if info, err := os.Stat(a.coreBinary); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("Mihomo binary is missing or not executable")
	}
	if err := os.MkdirAll(a.coreDataDir, 0o700); err != nil {
		return errors.New("Mihomo data directory is unavailable")
	}
	logFile, err := a.openCoreLog()
	if err != nil {
		return errors.New("Mihomo log file is unavailable")
	}
	defer logFile.Close()
	command := exec.Command(a.coreBinary, "-t", "-d", a.coreDataDir, "-f", a.runtimeConfigPath)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Run(); err != nil {
		return errors.New("Mihomo configuration validation failed; see the private Core log")
	}
	return nil
}

func (a *manager) startCore() (*managedProcess, error) {
	logFile, err := a.openCoreLog()
	if err != nil {
		return nil, errors.New("Mihomo log file is unavailable")
	}
	defer logFile.Close()
	command := exec.Command(a.coreBinary, "-d", a.coreDataDir, "-f", a.runtimeConfigPath)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, errors.New("Mihomo Core could not start")
	}
	process := &managedProcess{cmd: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		process.finished = true
		process.mu.Unlock()
		close(process.done)
	}()
	return process, nil
}

func (a *manager) openCoreLog() (*os.File, error) {
	path := a.coreLogPath
	if path == "" {
		path = filepath.Join(a.coreDataDir, "mihomo.log")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (a *manager) waitForCore(ctx context.Context, process *managedProcess) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		_, err := a.coreRequest(requestCtx, http.MethodGet, "/version", nil)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-process.done:
			return fmt.Errorf("Mihomo Core exited before its API became ready: %w", process.exitError())
		case <-ctx.Done():
			return errors.New("Mihomo Core API did not become ready within 45 seconds")
		case <-ticker.C:
		}
	}
}

func (p *managedProcess) exitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		return errors.New("process exited")
	}
	return p.err
}

func (p *managedProcess) stop(timeout time.Duration) {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (a *manager) gatewayHandler() http.Handler {
	managerURL, _ := url.Parse("http://" + a.listenAddr)
	coreURL, _ := url.Parse(a.apiURL)
	managerProxy := httputil.NewSingleHostReverseProxy(managerURL)
	managerProxy.ErrorHandler = proxyError
	coreProxy := httputil.NewSingleHostReverseProxy(coreURL)
	coreProxy.ErrorHandler = proxyError
	dashboardHandler := a.dashboardHandler()
	coreProxy.Director = func(request *http.Request) {
		request.URL.Scheme = coreURL.Scheme
		request.URL.Host = coreURL.Host
		request.Host = coreURL.Host
		request.Header.Set("Authorization", "Bearer "+a.apiSecret())
		request.Header.Set("Accept-Encoding", "identity")
	}

	prefix := strings.TrimRight(a.appPrefix, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Trim-Userid") == "" {
			http.Error(w, "fnOS login required", http.StatusUnauthorized)
			return
		}
		if !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Trim-Isadmin")), "true") {
			http.Error(w, "administrator access required", http.StatusForbidden)
			return
		}
		if r.URL.Path == prefix {
			http.Redirect(w, r, prefix+"/manager/", http.StatusFound)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		suffix := strings.TrimPrefix(r.URL.Path, prefix)
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = new(url.URL)
		*proxyRequest.URL = *r.URL
		proxyRequest.URL.RawPath = ""
		switch {
		case suffix == "/manager" || suffix == "/manager/":
			proxyRequest.URL.Path = "/"
			managerProxy.ServeHTTP(w, proxyRequest)
		case strings.HasPrefix(suffix, "/manager/"):
			proxyRequest.URL.Path = strings.TrimPrefix(suffix, "/manager")
			managerProxy.ServeHTTP(w, proxyRequest)
		case suffix == "/ui":
			http.Redirect(w, r, prefix+"/ui/", http.StatusFound)
		case strings.HasPrefix(suffix, "/ui/"):
			proxyRequest.URL.Path = strings.TrimPrefix(suffix, "/ui")
			dashboardHandler.ServeHTTP(w, proxyRequest)
		case suffix == "/healthz":
			proxyRequest.URL.Path = suffix
			managerProxy.ServeHTTP(w, proxyRequest)
		default:
			proxyRequest.URL.Path = suffix
			if proxyRequest.URL.Path == "" {
				proxyRequest.URL.Path = "/"
			}
			coreProxy.ServeHTTP(w, proxyRequest)
		}
	})
}

func (a *manager) dashboardHandler() http.Handler {
	files := http.FileServer(http.Dir(a.dashboardDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			files.ServeHTTP(w, r)
			return
		}
		data, err := os.ReadFile(filepath.Join(a.dashboardDir, "index.html"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		body := bytes.Replace(data, []byte("</head>"), []byte(a.dashboardInjection()), 1)
		if bytes.Equal(body, data) {
			http.Error(w, "dashboard index is missing a closing head tag", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
}

func proxyError(w http.ResponseWriter, _ *http.Request, _ error) {
	http.Error(w, "upstream service unavailable", http.StatusBadGateway)
}

func (a *manager) apiSecret() string {
	data, err := os.ReadFile(a.secretFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (a *manager) dashboardInjection() string {
	prefix, _ := json.Marshal(strings.TrimRight(a.appPrefix, "/"))
	script := `<script>(()=>{const base=__APP_PREFIX__,u=new URL(location.href),q=u.searchParams;if(!q.has('hostname')){q.set('hostname',location.hostname);q.set('port',location.port||(location.protocol==='https:'?'443':'80'));q.delete('http');q.delete('https');q.set(location.protocol==='https:'?'https':'http','1');q.set('secondaryPath',base);q.set('label','ShellCrash (fnOS)');location.replace(u.toString());return}const addBack=()=>{if(!document.body||document.getElementById('shellcrash-fnos-back'))return;const a=document.createElement('a');a.id='shellcrash-fnos-back';a.href=base+'/manager/';a.textContent='← 返回 ShellCrash 订阅管理';a.setAttribute('aria-label','返回 ShellCrash 订阅管理');a.style.cssText='position:fixed;right:18px;bottom:18px;z-index:2147483647;display:inline-block;border-radius:999px;padding:10px 15px;background:#146ef5;color:white;text-decoration:none;font:600 13px/1.2 -apple-system,BlinkMacSystemFont,Segoe UI,sans-serif;box-shadow:0 8px 24px rgba(0,0,0,.2)';document.body.appendChild(a)};if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',addBack,{once:true});else addBack()})()</script></head>`
	return strings.Replace(script, "__APP_PREFIX__", string(prefix), 1)
}
