package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	providerName = "fnos-subscription"
	maxBodyBytes = 10 << 20
	maxRequest   = 64 << 10
	maxURLLength = 4096
	minInterval  = 1
	maxInterval  = 720
)

var errMihomoUnavailable = errors.New("Mihomo API 暂不可用。")

var buildVersion = "dev"

//go:embed web/*
var webFiles embed.FS

type manager struct {
	configPath        string
	runtimeConfigPath string
	settingsPath      string
	overlayPath       string
	profileBackupPath string
	runtimeBackupPath string
	shellConfigPath   string
	shellBackupPath   string
	apiURL            string
	secretFile        string
	listenAddr        string
	coreBinary        string
	coreDataDir       string
	coreLogPath       string
	gatewaySocket     string
	appPrefix         string
	dashboardDir      string
	controllerAddr    string
	mixedPort         int
	client            *http.Client

	mu       sync.Mutex
	resched  chan struct{}
	version  string
	coreHTTP *http.Client
}

type subscriptionSettings struct {
	URL           string    `json:"url"`
	IntervalHours int       `json:"intervalHours"`
	SchemaVersion int       `json:"schemaVersion,omitempty"`
	LastAttempt   time.Time `json:"lastAttempt,omitempty"`
	LastSuccess   time.Time `json:"lastSuccess,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
	ProxyCount    int       `json:"proxyCount,omitempty"`
	ProviderCount int       `json:"providerCount,omitempty"`
}

type groupInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type statusResponse struct {
	Configured                bool        `json:"configured"`
	Conflict                  bool        `json:"conflict"`
	CoreConnected             bool        `json:"coreConnected"`
	TUNActive                 bool        `json:"tunActive"`
	ProxyPort                 int         `json:"proxyPort,omitempty"`
	Host                      string      `json:"host,omitempty"`
	IntervalHours             int         `json:"intervalHours,omitempty"`
	ProxyCount                int         `json:"proxyCount,omitempty"`
	ProviderCount             int         `json:"providerCount,omitempty"`
	UnreferencedProviderCount int         `json:"unreferencedProviderCount,omitempty"`
	OverlayYAML               string      `json:"overlayYaml,omitempty"`
	DNSMode                   string      `json:"dnsMode"`
	TUNMode                   string      `json:"tunMode"`
	LastAttempt               string      `json:"lastAttempt,omitempty"`
	LastSuccess               string      `json:"lastSuccess,omitempty"`
	LastError                 string      `json:"lastError,omitempty"`
	Groups                    []groupInfo `json:"groups"`
	Version                   string      `json:"version"`
}

type subscriptionRequest struct {
	URL           string  `json:"url"`
	IntervalHours int     `json:"intervalHours"`
	Group         string  `json:"group,omitempty"`
	OverlayYAML   *string `json:"overlayYaml,omitempty"`
	DNSMode       string  `json:"dnsMode,omitempty"`
	TUNMode       string  `json:"tunMode,omitempty"`
}

type featureModes struct {
	dns string
	tun string
}

type apiError struct {
	Error string `json:"error"`
}

type fileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func main() {
	app := &manager{
		configPath:        envOr("CONFIG_PATH", "/etc/ShellCrash/yamls/config.yaml"),
		runtimeConfigPath: envOr("RUNTIME_CONFIG_PATH", "/tmp/ShellCrash/config.yaml"),
		settingsPath:      envOr("SETTINGS_PATH", "/etc/ShellCrash/configs/subscription-manager.json"),
		overlayPath:       envOr("OVERLAY_PATH", "/etc/ShellCrash/configs/fnos-overrides.yaml"),
		profileBackupPath: envOr("PROFILE_BACKUP_PATH", "/etc/ShellCrash/configs/fnos-subscription-profile.backup.yaml"),
		runtimeBackupPath: envOr("RUNTIME_BACKUP_PATH", "/etc/ShellCrash/configs/fnos-subscription-runtime.backup.yaml"),
		shellConfigPath:   envOr("SHELLCRASH_CONFIG_PATH", "/etc/ShellCrash/configs/ShellCrash.cfg"),
		shellBackupPath:   envOr("SHELLCRASH_BACKUP_PATH", "/etc/ShellCrash/configs/fnos-subscription-shellcrash-settings.backup"),
		apiURL:            strings.TrimRight(envOr("API_URL", "http://shellcrash:9999"), "/"),
		secretFile:        envOr("API_SECRET_FILE", "/run/secrets/shellcrash-api.secret"),
		listenAddr:        envOr("LISTEN_ADDR", "127.0.0.1:9998"),
		coreBinary:        strings.TrimSpace(os.Getenv("CORE_BINARY")),
		coreDataDir:       strings.TrimSpace(os.Getenv("CORE_DATA_DIR")),
		coreLogPath:       strings.TrimSpace(os.Getenv("CORE_LOG_PATH")),
		gatewaySocket:     strings.TrimSpace(os.Getenv("GATEWAY_SOCKET")),
		appPrefix:         envOr("APP_PREFIX", "/app/shellcrash-fnos"),
		dashboardDir:      strings.TrimSpace(os.Getenv("DASHBOARD_DIR")),
		controllerAddr:    envOr("CORE_CONTROLLER", coreControllerAddress),
		client:            newSafeHTTPClient(),
		resched:           make(chan struct{}, 1),
		version:           envOr("MANAGER_VERSION", buildVersion),
		coreHTTP:          &http.Client{Timeout: 15 * time.Second},
	}
	if port, err := strconv.Atoi(envOr("MIXED_PORT", "17890")); err == nil && port > 0 && port <= 65535 {
		app.mixedPort = port
	} else {
		log.Fatal("invalid MIXED_PORT")
	}
	if app.coreBinary != "" {
		if err := app.runNative(); err != nil {
			log.Fatal("native ShellCrash service stopped: ", err)
		}
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/subscription", app.handleSubscription)
	mux.HandleFunc("/api/refresh", app.handleRefresh)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", app.handleWeb)

	server := &http.Server{
		Addr:              app.listenAddr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go app.runScheduler()
	log.Printf("subscription manager %s listening on %s", app.version, app.listenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("subscription manager server stopped")
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *manager) handleWeb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := "web/index.html"
	contentType := "text/html; charset=utf-8"
	switch r.URL.Path {
	case "/", "/index.html":
	case "/app.js":
		name, contentType = "web/app.js", "text/javascript; charset=utf-8"
	case "/style.css":
		name, contentType = "web/style.css", "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := webFiles.ReadFile(name)
	if err != nil {
		http.Error(w, "web assets unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

func (a *manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result, err := a.statusLocked()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ShellCrash 配置读取失败，请检查配置文件权限或格式。")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *manager) statusLocked() (statusResponse, error) {
	configData, err := os.ReadFile(a.configPath)
	if err != nil {
		return statusResponse{}, err
	}
	_, root, err := decodeConfig(configData)
	if err != nil {
		return statusResponse{}, err
	}
	groups := readGroups(root)
	settings, settingsExist, err := a.readSettings()
	if err != nil {
		return statusResponse{}, err
	}
	_, providerPresent := mappingEntry(mappingValue(root, "proxy-providers"), providerName)
	result := statusResponse{
		Configured: settingsExist && settings.URL != "",
		Conflict:   providerPresent && !settingsExist,
		Groups:     groups,
		Version:    a.version,
		DNSMode:    "inherit",
		TUNMode:    "inherit",
	}
	result.ProviderCount = proxyProviderCount(root)
	result.UnreferencedProviderCount = unreferencedProxyProviderCount(root)
	if overlay, readErr := os.ReadFile(a.overlayPath); readErr == nil {
		result.OverlayYAML = string(overlay)
		if _, overlayRoot, decodeErr := decodeOverlay(overlay); decodeErr == nil {
			result.DNSMode = featureMode(overlayRoot, "dns")
			result.TUNMode = featureMode(overlayRoot, "tun")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return statusResponse{}, readErr
	}
	if settingsExist {
		parsed, parseErr := url.Parse(settings.URL)
		if parseErr == nil {
			result.Host = parsed.Hostname()
		}
		result.IntervalHours = settings.IntervalHours
		result.ProxyCount = settings.ProxyCount
		if settings.ProviderCount > 0 {
			result.ProviderCount = settings.ProviderCount
		}
		if !settings.LastAttempt.IsZero() {
			result.LastAttempt = settings.LastAttempt.UTC().Format(time.RFC3339)
		}
		if !settings.LastSuccess.IsZero() {
			result.LastSuccess = settings.LastSuccess.UTC().Format(time.RFC3339)
		}
		result.LastError = settings.LastError
	}
	if a.coreBinary != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if body, coreErr := a.coreRequest(ctx, http.MethodGet, "/configs", nil); coreErr == nil {
			var active struct {
				MixedPort int `json:"mixed-port"`
				TUN       struct {
					Enable bool   `json:"enable"`
					Device string `json:"device"`
				} `json:"tun"`
			}
			if json.Unmarshal(body, &active) == nil {
				result.CoreConnected = true
				result.ProxyPort = active.MixedPort
				device := strings.TrimSpace(active.TUN.Device)
				if device == "" {
					device = "Meta"
				}
				if active.TUN.Enable {
					if networkInterface, interfaceErr := net.InterfaceByName(device); interfaceErr == nil && networkInterface.Flags&net.FlagUp != 0 {
						result.TUNActive = true
					}
				}
			}
		}
	}
	return result, nil
}

func (a *manager) handleSubscription(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.saveSubscription(w, r)
	case http.MethodDelete:
		a.deleteSubscription(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *manager) saveSubscription(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input subscriptionRequest
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "订阅设置格式错误。")
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "订阅设置格式错误。")
		return
	}
	if !validFeatureMode(input.DNSMode) || !validFeatureMode(input.TUNMode) {
		writeAPIError(w, http.StatusBadRequest, "DNS/TUN 模式须为 inherit、on 或 off。")
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	current, currentExists, err := a.readSettings()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "订阅设置读取失败。")
		return
	}
	if input.URL == "" && !currentExists {
		writeAPIError(w, http.StatusBadRequest, "请先填写 Clash/Mihomo YAML 订阅链接。")
		return
	}
	if input.URL == "" {
		input.URL = current.URL
	}
	if strings.TrimSpace(input.URL) != input.URL || len(input.URL) > maxURLLength {
		writeAPIError(w, http.StatusBadRequest, "订阅链接格式无效。")
		return
	}
	if input.IntervalHours == 0 && currentExists {
		input.IntervalHours = current.IntervalHours
	}
	if input.IntervalHours < minInterval || input.IntervalHours > maxInterval {
		writeAPIError(w, http.StatusBadRequest, "自动更新间隔须为 1 到 720 小时。")
		return
	}
	newSettings := current
	newSettings.URL = input.URL
	newSettings.IntervalHours = input.IntervalHours
	newSettings.SchemaVersion = 2
	var requestedModes *featureModes
	if input.DNSMode != "" || input.TUNMode != "" {
		requestedModes = &featureModes{dns: input.DNSMode, tun: input.TUNMode}
	}
	if err := a.syncSubscriptionLocked(r.Context(), &newSettings, input.OverlayYAML, requestedModes); err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.signalReschedule()
	result, statusErr := a.statusLocked()
	if statusErr != nil {
		writeJSON(w, http.StatusOK, map[string]string{"message": "订阅已保存并载入。"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *manager) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, exists, err := a.readSettings()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "订阅设置读取失败，未修改。")
		return
	}
	if !exists {
		writeJSON(w, http.StatusOK, map[string]string{"message": "没有已保存的订阅。"})
		return
	}
	configSnapshot, err := snapshotFile(a.configPath)
	if err != nil || !configSnapshot.exists {
		writeAPIError(w, http.StatusInternalServerError, "ShellCrash 配置读取失败，未修改。")
		return
	}
	runtimeSnapshot, err := snapshotFile(a.runtimeConfigPath)
	if err != nil || !runtimeSnapshot.exists {
		writeAPIError(w, http.StatusServiceUnavailable, "无法保存 ShellCrash 当前运行配置，未修改。")
		return
	}
	settingsSnapshot, err := snapshotFile(a.settingsPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "订阅设置读取失败，未修改。")
		return
	}
	overlaySnapshot, err := snapshotFile(a.overlayPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "覆盖配置读取失败，未修改。")
		return
	}
	profileBackup, err := snapshotFile(a.profileBackupPath)
	if err != nil || !profileBackup.exists {
		writeAPIError(w, http.StatusConflict, "找不到订阅前的配置备份，已停止以保护当前配置。")
		return
	}
	runtimeBackup, err := snapshotFile(a.runtimeBackupPath)
	if err != nil || !runtimeBackup.exists {
		writeAPIError(w, http.StatusConflict, "找不到订阅前的运行配置备份，已停止以保护当前配置。")
		return
	}
	shellConfigSnapshot, err := snapshotFile(a.shellConfigPath)
	if err != nil || !shellConfigSnapshot.exists {
		writeAPIError(w, http.StatusConflict, "找不到 ShellCrash 设置文件，未修改。")
		return
	}
	shellBackupSnapshot, err := snapshotFile(a.shellBackupPath)
	if err != nil || !shellBackupSnapshot.exists {
		writeAPIError(w, http.StatusConflict, "找不到订阅前的 ShellCrash 设置备份，已停止以保护现有配置。")
		return
	}
	profileBackupSnapshot, err := snapshotFile(a.profileBackupPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "订阅前配置备份读取失败，未修改。")
		return
	}
	runtimeBackupSnapshot, err := snapshotFile(a.runtimeBackupPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "订阅前运行配置备份读取失败，未修改。")
		return
	}
	rollback := func() {
		_ = restoreSnapshot(configSnapshot)
		_ = restoreSnapshot(runtimeSnapshot)
		_ = restoreSnapshot(settingsSnapshot)
		_ = restoreSnapshot(overlaySnapshot)
		_ = restoreSnapshot(profileBackupSnapshot)
		_ = restoreSnapshot(runtimeBackupSnapshot)
		_ = restoreSnapshot(shellConfigSnapshot)
		_ = restoreSnapshot(shellBackupSnapshot)
	}
	if err := atomicWrite(a.configPath, profileBackup.data, configSnapshot.mode); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "无法恢复订阅前的 ShellCrash 配置。")
		return
	}
	if err := atomicWrite(a.runtimeConfigPath, runtimeBackup.data, runtimeSnapshot.mode); err != nil {
		rollback()
		writeAPIError(w, http.StatusInternalServerError, "无法恢复订阅前的运行配置，当前配置已回滚。")
		return
	}
	if err := atomicWrite(a.shellConfigPath, shellBackupSnapshot.data, shellConfigSnapshot.mode); err != nil {
		rollback()
		writeAPIError(w, http.StatusInternalServerError, "无法恢复订阅前的 ShellCrash 设置，当前配置已回滚。")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.reloadConfig(ctx, runtimeBackup.data); err != nil {
		rollback()
		_ = a.reloadConfig(context.Background(), runtimeSnapshot.data)
		writeAPIError(w, http.StatusServiceUnavailable, "Mihomo 重载失败，已恢复原有配置。")
		return
	}
	for _, path := range []string{a.settingsPath, a.overlayPath, a.profileBackupPath, a.runtimeBackupPath, a.shellBackupPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollback()
			_ = a.reloadConfig(context.Background(), runtimeSnapshot.data)
			writeAPIError(w, http.StatusInternalServerError, "订阅设置清理失败，当前配置已回滚。")
			return
		}
	}
	a.signalReschedule()
	writeJSON(w, http.StatusOK, map[string]string{"message": "订阅已移除，订阅前的配置已恢复。"})
}

func (a *manager) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	settings, exists, err := a.readSettings()
	if err != nil || !exists || settings.URL == "" {
		writeAPIError(w, http.StatusBadRequest, "尚未配置订阅链接。")
		return
	}
	if err := a.refreshLocked(r.Context(), &settings); err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.signalReschedule()
	result, err := a.statusLocked()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"message": "订阅已更新。"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *manager) refreshLocked(ctx context.Context, settings *subscriptionSettings) error {
	if err := a.syncSubscriptionLocked(ctx, settings, nil, nil); err != nil {
		settings.LastAttempt = time.Now().UTC()
		settings.LastError = err.Error()
		_ = a.saveSettings(*settings)
		return err
	}
	return nil
}

type providerSource struct {
	name     string
	typeName string
}

type subscriptionProfileInfo struct {
	proxyCount    int
	providerCount int
	providers     []providerSource
	fullProfile   bool
}

func (a *manager) syncSubscriptionLocked(ctx context.Context, settings *subscriptionSettings, requestedOverlay *string, requestedModes *featureModes) error {
	fetched, err := a.fetchSubscription(ctx, settings.URL)
	if err != nil {
		return err
	}
	profileInfo, err := inspectSubscriptionProfile(fetched)
	if err != nil {
		return err
	}

	configSnapshot, err := snapshotFile(a.configPath)
	if err != nil || !configSnapshot.exists {
		return errors.New("ShellCrash 持久配置不可读取。")
	}
	runtimeSnapshot, err := snapshotFile(a.runtimeConfigPath)
	if err != nil {
		return errors.New("ShellCrash 当前运行配置不可读取。")
	}
	if !runtimeSnapshot.exists && !profileInfo.fullProfile {
		return errors.New("ShellCrash 当前运行配置尚未就绪。")
	}
	settingsSnapshot, err := snapshotFile(a.settingsPath)
	if err != nil {
		return errors.New("订阅设置不可读取。")
	}
	overlaySnapshot, err := snapshotFile(a.overlayPath)
	if err != nil {
		return errors.New("DNS/TUN 覆盖配置不可读取。")
	}
	profileBackup, err := snapshotFile(a.profileBackupPath)
	if err != nil {
		return errors.New("无法读取订阅前配置备份。")
	}
	runtimeBackup, err := snapshotFile(a.runtimeBackupPath)
	if err != nil {
		return errors.New("无法读取订阅前运行配置备份。")
	}
	if profileBackup.exists != runtimeBackup.exists {
		return errors.New("订阅前配置备份不完整，已停止以保护现有配置。")
	}
	shellConfigSnapshot, err := snapshotFile(a.shellConfigPath)
	if err != nil || !shellConfigSnapshot.exists {
		return errors.New("ShellCrash 运行设置不可读取。")
	}
	shellBackup, err := snapshotFile(a.shellBackupPath)
	if err != nil {
		return errors.New("无法读取订阅前 ShellCrash 设置备份。")
	}
	updatedShellConfig := shellConfigSnapshot.data
	if profileInfo.fullProfile {
		updatedShellConfig, err = setShellConfigValue(shellConfigSnapshot.data, "disoverride", "1")
		if err != nil {
			return errors.New("ShellCrash 覆盖模式设置无法生成。")
		}
	}

	var overlayData []byte
	switch {
	case requestedOverlay != nil:
		overlayData = []byte(*requestedOverlay)
	case overlaySnapshot.exists:
		overlayData = overlaySnapshot.data
	default:
		overlayData, err = overlayFromSubscription(fetched)
		if err != nil {
			return err
		}
	}
	overlayDoc, overlayRoot, err := decodeOverlay(overlayData)
	if err != nil {
		return err
	}
	if requestedModes != nil {
		_, subscriptionRoot, decodeErr := decodeConfig(fetched)
		if decodeErr != nil {
			return errors.New("无法读取订阅中的 DNS/TUN 配置。")
		}
		if err := applyFeatureModes(overlayRoot, subscriptionRoot, *requestedModes); err != nil {
			return err
		}
		normalizeTunStack(overlayRoot)
	}
	cleanOverlay, err := encodeConfig(overlayDoc)
	if err != nil {
		return errors.New("DNS/TUN 覆盖配置无法生成。")
	}
	mergedProfile, mergedRuntime, err := mergeSubscriptionConfig(configSnapshot.data, runtimeSnapshot.data, fetched, cleanOverlay)
	if err != nil {
		return err
	}
	if !fileMatchesSnapshot(configSnapshot) || !fileMatchesSnapshot(runtimeSnapshot) || !fileMatchesSnapshot(settingsSnapshot) || !fileMatchesSnapshot(overlaySnapshot) || !fileMatchesSnapshot(profileBackup) || !fileMatchesSnapshot(runtimeBackup) || !fileMatchesSnapshot(shellConfigSnapshot) || !fileMatchesSnapshot(shellBackup) {
		return errors.New("配置在操作期间发生变化，已停止写入以保护最新内容。请刷新后重试。")
	}

	backupCreated := false
	shellBackupCreated := false
	if !profileBackup.exists {
		if err := atomicWrite(a.profileBackupPath, configSnapshot.data, 0o600); err != nil {
			return errors.New("无法备份订阅前的 ShellCrash 配置。")
		}
		runtimeBackupData := runtimeSnapshot.data
		if !runtimeSnapshot.exists {
			runtimeBackupData = configSnapshot.data
		} else if _, _, decodeErr := decodeConfig(runtimeSnapshot.data); decodeErr != nil {
			runtimeBackupData = configSnapshot.data
		}
		if err := atomicWrite(a.runtimeBackupPath, runtimeBackupData, 0o600); err != nil {
			_ = os.Remove(a.profileBackupPath)
			return errors.New("无法备份订阅前的运行配置。")
		}
		backupCreated = true
	}
	if !shellBackup.exists {
		if err := atomicWrite(a.shellBackupPath, shellConfigSnapshot.data, 0o600); err != nil {
			if backupCreated {
				_ = os.Remove(a.profileBackupPath)
				_ = os.Remove(a.runtimeBackupPath)
			}
			return errors.New("无法备份订阅前的 ShellCrash 设置。")
		}
		shellBackupCreated = true
	}

	newSettings := *settings
	newSettings.SchemaVersion = 2
	newSettings.LastAttempt = time.Now().UTC()
	newSettings.LastError = ""
	newSettings.ProxyCount = profileInfo.proxyCount
	newSettings.ProviderCount = profileInfo.providerCount
	rollback := func() {
		_ = restoreSnapshot(configSnapshot)
		_ = restoreSnapshot(runtimeSnapshot)
		_ = restoreSnapshot(settingsSnapshot)
		_ = restoreSnapshot(overlaySnapshot)
		_ = restoreSnapshot(shellConfigSnapshot)
		if backupCreated {
			_ = os.Remove(a.profileBackupPath)
			_ = os.Remove(a.runtimeBackupPath)
		}
		if shellBackupCreated {
			_ = os.Remove(a.shellBackupPath)
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = a.reloadConfig(rollbackCtx, runtimeSnapshot.data)
		cancel()
	}
	if err := atomicWrite(a.overlayPath, cleanOverlay, 0o600); err != nil {
		rollback()
		return errors.New("DNS/TUN 覆盖配置写入失败。")
	}
	if err := atomicWrite(a.configPath, mergedProfile, configSnapshot.mode); err != nil {
		rollback()
		return errors.New("ShellCrash 配置写入失败，现有配置已恢复。")
	}
	if err := atomicWrite(a.runtimeConfigPath, mergedRuntime, runtimeSnapshot.mode); err != nil {
		rollback()
		return errors.New("ShellCrash 运行配置写入失败，现有配置已恢复。")
	}
	if err := atomicWrite(a.shellConfigPath, updatedShellConfig, shellConfigSnapshot.mode); err != nil {
		rollback()
		return errors.New("ShellCrash 覆盖模式写入失败，现有配置已恢复。")
	}
	settingsData, _ := json.MarshalIndent(newSettings, "", "  ")
	settingsData = append(settingsData, '\n')
	if err := atomicWrite(a.settingsPath, settingsData, 0o600); err != nil {
		rollback()
		return errors.New("订阅设置写入失败，现有配置已恢复。")
	}
	if err := a.reloadConfig(ctx, mergedRuntime); err != nil {
		if profileInfo.fullProfile && errors.Is(err, errMihomoUnavailable) {
			newSettings.LastError = "配置已安全保存；Mihomo 尚未就绪，ShellCrash 启动后会加载该配置并自动重试。"
			if err := a.saveSettings(newSettings); err != nil {
				rollback()
				return errors.New("Mihomo 尚未就绪且订阅状态保存失败，当前配置已恢复。")
			}
			*settings = newSettings
			return nil
		}
		rollback()
		return fmt.Errorf("Mihomo 重载失败，已恢复订阅前的配置：%w", err)
	}

	providerNodeCount := 0
	for _, provider := range profileInfo.providers {
		if provider.typeName != "http" {
			continue
		}
		if err := a.updateProvider(ctx, provider.name); err != nil {
			rollback()
			return errors.New("Mihomo 无法刷新订阅中的 provider，已恢复订阅前的配置。")
		}
		count, err := a.providerProxyCount(ctx, provider.name)
		if err != nil {
			rollback()
			return errors.New("Mihomo 无法读取订阅 provider 的节点状态，已恢复订阅前的配置。")
		}
		providerNodeCount += count
	}
	hasHTTPProvider := false
	for _, provider := range profileInfo.providers {
		if provider.typeName == "http" {
			hasHTTPProvider = true
			break
		}
	}
	if hasHTTPProvider && providerNodeCount == 0 {
		rollback()
		return errors.New("订阅中的 provider 没有加载出可用节点，已恢复订阅前的配置。")
	}
	newSettings.ProxyCount = profileInfo.proxyCount + providerNodeCount
	newSettings.LastSuccess = time.Now().UTC()
	newSettings.LastAttempt = newSettings.LastSuccess
	settingsData, _ = json.MarshalIndent(newSettings, "", "  ")
	settingsData = append(settingsData, '\n')
	if err := atomicWrite(a.settingsPath, settingsData, 0o600); err != nil {
		rollback()
		return errors.New("订阅状态写入失败，已恢复上一次配置。")
	}
	*settings = newSettings
	return nil
}

func fileMatchesSnapshot(snapshot fileSnapshot) bool {
	current, err := snapshotFile(snapshot.path)
	return err == nil && current.exists == snapshot.exists && current.mode == snapshot.mode && bytes.Equal(current.data, snapshot.data)
}

func inspectSubscriptionProfile(data []byte) (subscriptionProfileInfo, error) {
	_, root, err := decodeConfig(data)
	if err != nil {
		return subscriptionProfileInfo{}, errors.New("订阅内容不是有效的 Clash/Mihomo YAML 配置。")
	}
	info := subscriptionProfileInfo{}
	info.fullProfile = isFullSubscriptionProfile(root)
	if proxies, exists := mappingEntry(root, "proxies"); exists {
		if proxies.Kind != yaml.SequenceNode {
			return info, errors.New("订阅中的 proxies 必须是列表。")
		}
		for _, proxy := range proxies.Content {
			if proxy.Kind != yaml.MappingNode || scalarValue(mappingValue(proxy, "name")) == "" || scalarValue(mappingValue(proxy, "type")) == "" {
				return info, errors.New("订阅中的代理节点格式不完整。")
			}
		}
		info.proxyCount = len(proxies.Content)
	}
	if providers, exists := mappingEntry(root, "proxy-providers"); exists {
		if providers.Kind != yaml.MappingNode {
			return info, errors.New("订阅中的 proxy-providers 必须是映射。")
		}
		for index := 0; index+1 < len(providers.Content); index += 2 {
			nameNode, value := providers.Content[index], providers.Content[index+1]
			name := scalarValue(nameNode)
			if name == "" || value.Kind != yaml.MappingNode {
				return info, errors.New("订阅中的 provider 格式不完整。")
			}
			typeName := scalarValue(mappingValue(value, "type"))
			switch typeName {
			case "http":
				if err := validatePublicURL(scalarValue(mappingValue(value, "url"))); err != nil {
					return info, errors.New("订阅中的 HTTP provider 地址无效或不是公网地址。")
				}
			case "file":
				path := scalarValue(mappingValue(value, "path"))
				if path == "" || filepath.IsAbs(path) || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
					return info, errors.New("订阅中的 file provider 路径无效。")
				}
			case "inline":
				payload := mappingValue(value, "payload")
				if payload == nil || payload.Kind != yaml.SequenceNode {
					return info, errors.New("订阅中的 inline provider 缺少 payload 列表。")
				}
			default:
				return info, errors.New("订阅包含不支持的 provider 类型。")
			}
			info.providers = append(info.providers, providerSource{name: name, typeName: typeName})
		}
		info.providerCount = len(info.providers)
	}
	if info.proxyCount == 0 && info.providerCount == 0 {
		return info, errors.New("订阅没有 proxies 或 proxy-providers。")
	}
	if ruleProviders, exists := mappingEntry(root, "rule-providers"); exists {
		if ruleProviders.Kind != yaml.MappingNode {
			return info, errors.New("订阅中的 rule-providers 必须是映射。")
		}
		for index := 1; index < len(ruleProviders.Content); index += 2 {
			provider := ruleProviders.Content[index]
			if provider.Kind != yaml.MappingNode || scalarValue(mappingValue(provider, "type")) != "http" {
				continue
			}
			if err := validatePublicURL(scalarValue(mappingValue(provider, "url"))); err != nil {
				return info, errors.New("订阅中的 HTTP 规则 provider 地址无效或不是公网地址。")
			}
		}
	}
	sort.Slice(info.providers, func(i, j int) bool { return info.providers[i].name < info.providers[j].name })
	return info, nil
}

func isFullSubscriptionProfile(root *yaml.Node) bool {
	for _, key := range []string{"proxy-providers", "proxy-groups", "rule-providers", "rules"} {
		if mappingValue(root, key) != nil {
			return true
		}
	}
	return false
}

func overlayFromSubscription(data []byte) ([]byte, error) {
	_, root, err := decodeConfig(data)
	if err != nil {
		return nil, errors.New("无法从订阅中读取 DNS/TUN 覆盖配置。")
	}
	overlay := mappingNode()
	for _, key := range []string{"dns", "tun"} {
		value, exists := mappingEntry(root, key)
		if !exists {
			continue
		}
		if value.Kind != yaml.MappingNode {
			return nil, errors.New("订阅中的 DNS/TUN 配置必须是映射。")
		}
		setMappingValue(overlay, key, cloneYAMLNode(value))
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{overlay}}
	return encodeConfig(doc)
}

func decodeOverlay(data []byte) (*yaml.Node, *yaml.Node, error) {
	if strings.TrimSpace(string(data)) == "" {
		root := mappingNode()
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}, root, nil
	}
	doc, root, err := decodeConfig(data)
	if err != nil {
		return nil, nil, errors.New("DNS/TUN 覆盖必须是有效的 YAML 映射。")
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		key := scalarValue(root.Content[index])
		if key != "dns" && key != "tun" {
			return nil, nil, errors.New("覆盖配置只允许设置 dns 和 tun 两个顶层字段。")
		}
		if root.Content[index+1].Kind != yaml.MappingNode {
			return nil, nil, errors.New("覆盖中的 dns 和 tun 必须是 YAML 映射。")
		}
	}
	normalizeTunStack(root)
	return doc, root, nil
}

func validFeatureMode(value string) bool {
	switch value {
	case "", "inherit", "on", "off":
		return true
	default:
		return false
	}
}

func featureMode(root *yaml.Node, key string) string {
	section := mappingValue(root, key)
	if section == nil || section.Kind != yaml.MappingNode {
		return "inherit"
	}
	if strings.EqualFold(scalarValue(mappingValue(section, "enable")), "true") {
		return "on"
	}
	return "off"
}

func applyFeatureModes(overlayRoot, subscriptionRoot *yaml.Node, modes featureModes) error {
	for _, feature := range []struct {
		name string
		mode string
	}{
		{name: "dns", mode: modes.dns},
		{name: "tun", mode: modes.tun},
	} {
		if feature.mode == "" {
			continue
		}
		if !validFeatureMode(feature.mode) {
			return errors.New("DNS/TUN 模式须为 inherit、on 或 off。")
		}
		if feature.mode == "inherit" {
			removeMappingValue(overlayRoot, feature.name)
			continue
		}

		section := mappingValue(overlayRoot, feature.name)
		if section == nil {
			if source := mappingValue(subscriptionRoot, feature.name); source != nil {
				if source.Kind != yaml.MappingNode {
					return errors.New("订阅中的 DNS/TUN 配置必须是映射。")
				}
				section = cloneYAMLNode(source)
			} else {
				section = mappingNode()
			}
			setMappingValue(overlayRoot, feature.name, section)
		}
		if section.Kind != yaml.MappingNode {
			return errors.New("覆盖中的 dns 和 tun 必须是 YAML 映射。")
		}
		setMappingValue(section, "enable", boolNode(feature.mode == "on"))
	}
	return nil
}

func boolNode(value bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprintf("%t", value)}
}

func removeMappingValue(node *yaml.Node, key string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Kind == yaml.ScalarNode && node.Content[index].Value == key {
			node.Content = append(node.Content[:index], node.Content[index+2:]...)
			return
		}
	}
}

func normalizeTunStack(root *yaml.Node) bool {
	tun := mappingValue(root, "tun")
	if tun == nil || tun.Kind != yaml.MappingNode || !strings.EqualFold(scalarValue(mappingValue(tun, "enable")), "true") {
		return false
	}
	changed := false
	stack := strings.ToLower(strings.TrimSpace(scalarValue(mappingValue(tun, "stack"))))
	if stack != "system" {
		setMappingValue(tun, "stack", stringNode("system"))
		changed = true
	}
	for _, key := range []string{"auto-route", "auto-redirect", "auto-detect-interface"} {
		if !strings.EqualFold(scalarValue(mappingValue(tun, key)), "true") {
			setMappingValue(tun, key, boolNode(true))
			changed = true
		}
	}
	changed = appendStringDefaults(tun, "route-exclude-address", []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"ff00::/8",
	}) || changed
	if strings.EqualFold(scalarValue(mappingValue(mappingValue(root, "dns"), "enable")), "true") {
		changed = appendStringDefaults(tun, "dns-hijack", []string{"any:53", "tcp://any:53"}) || changed
	}
	return changed
}

func appendStringDefaults(root *yaml.Node, key string, defaults []string) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	value := mappingValue(root, key)
	changed := false
	if value == nil {
		value = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMappingValue(root, key, value)
		changed = true
	} else if value.Kind != yaml.SequenceNode {
		original := cloneYAMLNode(value)
		value = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{original}}
		setMappingValue(root, key, value)
		changed = true
	}
	existing := make(map[string]bool, len(value.Content))
	for _, item := range value.Content {
		existing[scalarValue(item)] = true
	}
	for _, item := range defaults {
		if existing[item] {
			continue
		}
		value.Content = append(value.Content, stringNode(item))
		existing[item] = true
		changed = true
	}
	return changed
}

func mergeSubscriptionConfig(profileData, runtimeData, subscriptionData, overlayData []byte) ([]byte, []byte, error) {
	profileDoc, profileRoot, err := decodeConfig(profileData)
	if err != nil {
		return nil, nil, errors.New("ShellCrash 持久配置不是有效 YAML。")
	}
	profileProtected := cloneYAMLNode(profileRoot)
	runtimeDoc, runtimeRoot, runtimeErr := decodeConfig(runtimeData)
	_, subscriptionRoot, err := decodeConfig(subscriptionData)
	if err != nil {
		return nil, nil, errors.New("订阅配置不是有效 YAML。")
	}
	fullProfile := isFullSubscriptionProfile(subscriptionRoot)
	if runtimeErr != nil && !fullProfile {
		return nil, nil, errors.New("ShellCrash 运行配置不是有效 YAML。")
	}
	runtimeProtected := cloneYAMLNode(profileProtected)
	if runtimeErr == nil {
		runtimeProtected = cloneYAMLNode(runtimeRoot)
	} else {
		runtimeDoc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{cloneYAMLNode(subscriptionRoot)}}
		runtimeRoot = runtimeDoc.Content[0]
	}
	if fullProfile {
		profileDoc.Content[0] = cloneYAMLNode(subscriptionRoot)
		runtimeDoc.Content[0] = cloneYAMLNode(subscriptionRoot)
		profileRoot = profileDoc.Content[0]
		runtimeRoot = runtimeDoc.Content[0]
	} else {
		if err := mergeMapping(profileRoot, subscriptionRoot); err != nil {
			return nil, nil, errors.New("订阅配置无法合并到 ShellCrash 配置。")
		}
		if err := mergeMapping(runtimeRoot, subscriptionRoot); err != nil {
			return nil, nil, errors.New("订阅配置无法合并到运行配置。")
		}
	}
	for _, key := range []string{"mixed-port", "socks-port", "port", "redir-port", "tproxy-port", "allow-lan", "bind-address", "external-controller", "external-controller-cors", "external-controller-unix", "external-controller-pipe", "external-ui", "external-ui-url", "secret", "authentication"} {
		value := mappingValue(runtimeProtected, key)
		if value == nil {
			value = mappingValue(profileProtected, key)
		}
		if value != nil {
			setMappingValue(profileRoot, key, cloneYAMLNode(value))
			setMappingValue(runtimeRoot, key, cloneYAMLNode(value))
		}
	}
	_, overlayRoot, err := decodeOverlay(overlayData)
	if err != nil {
		return nil, nil, err
	}
	if err := mergeMapping(profileRoot, overlayRoot); err != nil {
		return nil, nil, errors.New("DNS/TUN 覆盖无法应用到持久配置。")
	}
	if err := mergeMapping(runtimeRoot, overlayRoot); err != nil {
		return nil, nil, errors.New("DNS/TUN 覆盖无法应用到运行配置。")
	}
	profileResult, err := encodeConfig(profileDoc)
	if err != nil {
		return nil, nil, errors.New("合并后的 ShellCrash 配置无法生成。")
	}
	runtimeResult, err := encodeConfig(runtimeDoc)
	if err != nil {
		return nil, nil, errors.New("合并后的 Mihomo 运行配置无法生成。")
	}
	return profileResult, runtimeResult, nil
}

func mergeMapping(destination, source *yaml.Node) error {
	if destination == nil || destination.Kind != yaml.MappingNode || source == nil || source.Kind != yaml.MappingNode {
		return errors.New("YAML root must be a mapping")
	}
	for index := 0; index+1 < len(source.Content); index += 2 {
		key := source.Content[index]
		if key.Kind != yaml.ScalarNode || key.Value == "" {
			return errors.New("YAML keys must be non-empty scalars")
		}
		setMappingValue(destination, key.Value, cloneYAMLNode(source.Content[index+1]))
	}
	return nil
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	return cloneYAMLNodeWithMap(node, map[*yaml.Node]*yaml.Node{})
}

func cloneYAMLNodeWithMap(node *yaml.Node, copies map[*yaml.Node]*yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if existing, ok := copies[node]; ok {
		return existing
	}
	copyNode := *node
	copyNode.Content = nil
	copyNode.Alias = nil
	copies[node] = &copyNode
	for _, child := range node.Content {
		copyNode.Content = append(copyNode.Content, cloneYAMLNodeWithMap(child, copies))
	}
	if node.Alias != nil {
		copyNode.Alias = cloneYAMLNodeWithMap(node.Alias, copies)
	}
	return &copyNode
}

func proxyProviderCount(root *yaml.Node) int {
	providers := mappingValue(root, "proxy-providers")
	if providers == nil || providers.Kind != yaml.MappingNode {
		return 0
	}
	return len(providers.Content) / 2
}

func unreferencedProxyProviderCount(root *yaml.Node) int {
	providers := mappingValue(root, "proxy-providers")
	groups := mappingValue(root, "proxy-groups")
	if providers == nil || providers.Kind != yaml.MappingNode {
		return 0
	}
	used := map[string]bool{}
	if groups != nil && groups.Kind == yaml.SequenceNode {
		for _, group := range groups.Content {
			if group.Kind != yaml.MappingNode {
				continue
			}
			includeAll := scalarValue(mappingValue(group, "include-all")) == "true" || scalarValue(mappingValue(group, "include-all-providers")) == "true"
			if includeAll {
				for index := 0; index+1 < len(providers.Content); index += 2 {
					used[scalarValue(providers.Content[index])] = true
				}
			}
			use := mappingValue(group, "use")
			if use == nil || use.Kind != yaml.SequenceNode {
				continue
			}
			for _, item := range use.Content {
				used[scalarValue(item)] = true
			}
		}
	}
	unreferenced := 0
	for index := 0; index+1 < len(providers.Content); index += 2 {
		if !used[scalarValue(providers.Content[index])] {
			unreferenced++
		}
	}
	return unreferenced
}

func (a *manager) runScheduler() {
	for {
		wait := time.Minute
		a.mu.Lock()
		settings, exists, err := a.readSettings()
		if err == nil && exists && settings.URL != "" {
			if settings.LastError != "" {
				wait = 5 * time.Minute
			} else if settings.SchemaVersion < 2 || settings.LastSuccess.IsZero() {
				wait = 0
			} else {
				wait = time.Until(settings.LastSuccess.Add(time.Duration(settings.IntervalHours) * time.Hour))
				if wait < 0 {
					wait = 0
				}
			}
		}
		a.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-a.resched:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			continue
		}

		a.mu.Lock()
		settings, exists, err = a.readSettings()
		if err == nil && exists && settings.URL != "" {
			if err := a.refreshLocked(context.Background(), &settings); err != nil {
				log.Printf("scheduled subscription refresh failed")
			}
		}
		a.mu.Unlock()
	}
}

func (a *manager) signalReschedule() {
	select {
	case a.resched <- struct{}{}:
	default:
	}
}

func (a *manager) readSettings() (subscriptionSettings, bool, error) {
	data, err := os.ReadFile(a.settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return subscriptionSettings{}, false, nil
	}
	if err != nil {
		return subscriptionSettings{}, false, err
	}
	var settings subscriptionSettings
	if len(data) > maxRequest || json.Unmarshal(data, &settings) != nil {
		return subscriptionSettings{}, false, errors.New("invalid subscription settings")
	}
	return settings, true, nil
}

func (a *manager) saveSettings(settings subscriptionSettings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(a.settingsPath, append(data, '\n'), 0o600)
}

func (a *manager) fetchSubscription(ctx context.Context, rawURL string) ([]byte, error) {
	if err := validateURL(rawURL); err != nil {
		return nil, errors.New("订阅链接必须是指向公网地址的 HTTP 或 HTTPS 链接。")
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("订阅链接格式无效。")
	}
	request.Header.Set("Accept", "application/yaml, text/yaml, text/plain, */*")
	request.Header.Set("User-Agent", "shellcrash-fnos-subscription-manager/1")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, errors.New("无法连接订阅服务器，请检查链接和 NAS 网络。")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("订阅服务器返回 HTTP %d。", response.StatusCode)
	}
	if response.ContentLength > maxBodyBytes {
		return nil, errors.New("订阅响应超过 10 MiB 限制。")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return nil, errors.New("订阅内容读取失败。")
	}
	if len(body) > maxBodyBytes {
		return nil, errors.New("订阅响应超过 10 MiB 限制。")
	}
	return body, nil
}

func (a *manager) reloadConfig(ctx context.Context, payload []byte) error {
	body, _ := json.Marshal(map[string]string{"payload": string(payload)})
	_, err := a.coreRequest(ctx, http.MethodPut, "/configs?force=true", body)
	return err
}

func (a *manager) updateProvider(ctx context.Context, provider string) error {
	_, err := a.coreRequest(ctx, http.MethodPut, "/providers/proxies/"+url.PathEscape(provider), nil)
	return err
}

func (a *manager) providerProxyCount(ctx context.Context, provider string) (int, error) {
	body, err := a.coreRequest(ctx, http.MethodGet, "/providers/proxies/"+url.PathEscape(provider), nil)
	if err != nil {
		return 0, err
	}
	var result struct {
		Proxies []json.RawMessage `json:"proxies"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, errors.New("Mihomo 返回了无法读取的代理提供者状态。")
	}
	return len(result.Proxies), nil
}

func (a *manager) coreRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	secret, err := os.ReadFile(a.secretFile)
	if err != nil {
		return nil, errors.New("Mihomo API 密钥不可用。")
	}
	secretValue := strings.TrimSpace(string(secret))
	if len(secretValue) < 32 || strings.ContainsAny(secretValue, "\r\n") {
		return nil, errors.New("Mihomo API 密钥格式无效。")
	}
	request, err := http.NewRequestWithContext(ctx, method, a.apiURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Mihomo API 请求无法创建。")
	}
	request.Header.Set("Authorization", "Bearer "+secretValue)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.coreHTTP.Do(request)
	if err != nil {
		return nil, errMihomoUnavailable
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := safeCoreError(responseBody, secretValue)
		if detail != "" {
			return nil, fmt.Errorf("Mihomo API 返回 HTTP %d：%s", response.StatusCode, detail)
		}
		return nil, fmt.Errorf("Mihomo API 返回 HTTP %d。", response.StatusCode)
	}
	return responseBody, nil
}

func setShellConfigValue(data []byte, key, value string) ([]byte, error) {
	if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(key) || !regexp.MustCompile(`^[a-zA-Z0-9._/-]*$`).MatchString(value) {
		return nil, errors.New("invalid ShellCrash config value")
	}
	text := string(data)
	lineEnding := "\n"
	if strings.Contains(text, "\r\n") {
		lineEnding = "\r\n"
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	hasFinalNewline := strings.HasSuffix(text, "\n")
	if hasFinalNewline {
		lines = lines[:len(lines)-1]
	}
	found := false
	settingPattern := regexp.MustCompile(`^[[:space:]]*` + regexp.QuoteMeta(key) + `[[:space:]]*=`)
	for index, line := range lines {
		if settingPattern.MatchString(line) {
			lines[index] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	result := strings.Join(lines, lineEnding)
	if hasFinalNewline || !found {
		result += lineEnding
	}
	return []byte(result), nil
}

func safeCoreError(body []byte, secret string) string {
	message := strings.TrimSpace(string(body))
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		for _, key := range []string{"message", "error", "details"} {
			if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
				message = strings.TrimSpace(value)
				break
			}
		}
	}
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	message = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`).ReplaceAllString(message, "[URL REDACTED]")
	message = regexp.MustCompile(`(?i)\b[a-f0-9]{32,}\b`).ReplaceAllString(message, "[TOKEN REDACTED]")
	message = strings.Map(func(value rune) rune {
		if value == '\n' || value == '\r' || value == '\t' {
			return ' '
		}
		if value < 0x20 || value == 0x7f {
			return -1
		}
		return value
	}, message)
	message = strings.TrimSpace(message)
	runes := []rune(message)
	if len(runes) > 240 {
		message = string(runes[:240]) + "…"
	}
	return message
}

func snapshotFile(path string) (fileSnapshot, error) {
	snapshot := fileSnapshot{path: path, mode: 0o600}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileSnapshot{}, err
	}
	snapshot.data = data
	snapshot.mode = info.Mode().Perm()
	snapshot.exists = true
	return snapshot, nil
}

func restoreSnapshot(snapshot fileSnapshot) error {
	if snapshot.exists {
		return atomicWrite(snapshot.path, snapshot.data, snapshot.mode)
	}
	if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".subscription-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode.Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func decodeConfig(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("configuration root must be a YAML mapping")
	}
	return &doc, doc.Content[0], nil
}

func encodeConfig(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func extractProxyList(data []byte) ([]byte, int, error) {
	_, root, err := decodeConfig(data)
	if err != nil {
		return nil, 0, errors.New("订阅内容不是有效的 Clash/Mihomo YAML。")
	}
	proxies := mappingValue(root, "proxies")
	if proxies == nil || proxies.Kind != yaml.SequenceNode || len(proxies.Content) == 0 {
		return nil, 0, errors.New("订阅内容没有可用的 proxies 节点。")
	}
	for _, proxy := range proxies.Content {
		if proxy.Kind != yaml.MappingNode || scalarValue(mappingValue(proxy, "name")) == "" || scalarValue(mappingValue(proxy, "type")) == "" {
			return nil, 0, errors.New("订阅代理列表格式不完整。")
		}
	}
	rootOut := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	rootOut.Content = append(rootOut.Content, stringNode("proxies"), proxies)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{rootOut}}
	encoded, err := encodeConfig(doc)
	if err != nil {
		return nil, 0, errors.New("订阅节点格式无法转换。")
	}
	return encoded, len(proxies.Content), nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Kind == yaml.ScalarNode && node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func mappingEntry(node *yaml.Node, key string) (*yaml.Node, bool) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, false
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Kind == yaml.ScalarNode && node.Content[index].Value == key {
			return node.Content[index+1], true
		}
	}
	return nil, false
}

func scalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func mappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Kind == yaml.ScalarNode && node.Content[index].Value == key {
			node.Content[index+1] = value
			return
		}
	}
	node.Content = append(node.Content, stringNode(key), value)
}

func readGroups(root *yaml.Node) []groupInfo {
	sequence := mappingValue(root, "proxy-groups")
	if sequence == nil || sequence.Kind != yaml.SequenceNode {
		return []groupInfo{}
	}
	groups := make([]groupInfo, 0, len(sequence.Content))
	for _, node := range sequence.Content {
		if node.Kind != yaml.MappingNode {
			continue
		}
		name := scalarValue(mappingValue(node, "name"))
		groupType := scalarValue(mappingValue(node, "type"))
		if name == "" || !supportedGroupType(groupType) {
			continue
		}
		groups = append(groups, groupInfo{Name: name, Type: groupType})
	}
	return groups
}

func supportedGroupType(value string) bool {
	switch value {
	case "select", "url-test", "fallback", "load-balance":
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiError{Error: message})
}

func validateURL(raw string) error {
	if raw == "" || len(raw) > maxURLLength {
		return errors.New("invalid URL")
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("invalid URL")
	}
	if strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".local") || strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".internal") || strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".lan") {
		return errors.New("private hostname")
	}
	if address, parseErr := netip.ParseAddr(strings.Trim(parsed.Hostname(), "[]")); parseErr == nil && !isPublicAddress(address.Unmap()) {
		return errors.New("non-public address")
	}
	return nil
}

func validatePublicURL(raw string) error {
	if err := validateURL(raw); err != nil {
		return err
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return errors.New("invalid URL")
	}
	_, err = publicAddresses(context.Background(), parsed.Hostname())
	return err
}

func newSafeHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 12 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := publicAddresses(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				lastErr = dialErr
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   35 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 || validateURL(request.URL.String()) != nil {
				return http.ErrUseLastResponse
			}
			if _, err := publicAddresses(request.Context(), request.URL.Hostname()); err != nil {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

var deniedRanges = func() []netip.Prefix {
	values := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "100::/64", "2001:db8::/32",
		"2001::/23", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}()

func publicAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if parsed, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !isPublicAddress(parsed) {
			return nil, errors.New("non-public address")
		}
		return []netip.Addr{parsed}, nil
	}
	if strings.Contains(host, "%") {
		return nil, errors.New("scoped IPv6 address rejected")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(resolved) == 0 {
		return nil, errors.New("host could not be resolved")
	}
	addresses := make([]netip.Addr, 0, len(resolved))
	for _, item := range resolved {
		ip, ok := netip.AddrFromSlice(item.IP)
		if !ok {
			return nil, errors.New("invalid resolved address")
		}
		ip = ip.Unmap()
		if !isPublicAddress(ip) {
			return nil, errors.New("non-public address")
		}
		addresses = append(addresses, ip)
	}
	return addresses, nil
}

func isPublicAddress(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, prefix := range deniedRanges {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
