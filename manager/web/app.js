const form = document.querySelector("#subscription-form");
const urlInput = document.querySelector("#subscription-url");
const intervalInput = document.querySelector("#interval-hours");
const overlayInput = document.querySelector("#overlay-yaml");
let dnsModeTouched = false;
let tunModeTouched = false;
const saveButton = document.querySelector("#save-button");
const refreshButton = document.querySelector("#refresh-button");
const deleteButton = document.querySelector("#delete-button");
const message = document.querySelector("#form-message");
let isConfigured = false;

async function api(path, options = {}) {
  const response = await fetch(path, {
    cache: "no-store",
    ...options,
    headers: {
      ...(options.body ? { "Content-Type": "application/json" } : {}),
      ...(options.headers || {}),
    },
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `请求失败（${response.status}）`);
  return body;
}

function formatDate(value) {
  if (!value) return "尚未更新";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "尚未更新" : date.toLocaleString("zh-CN");
}

function setMessage(text, isError = false) {
  message.textContent = text;
  message.classList.toggle("is-error", isError);
}

function selectedMode(name) {
  return document.querySelector('input[name="' + name + '"]:checked')?.value || "inherit";
}

function selectMode(name, value) {
  const option = document.querySelector('input[name="' + name + '"][value="' + value + '"]');
  if (option) option.checked = true;
}

document.querySelectorAll('input[name="dns-mode"]').forEach((input) => {
  input.addEventListener("change", () => { dnsModeTouched = true; });
});
document.querySelectorAll('input[name="tun-mode"]').forEach((input) => {
  input.addEventListener("change", () => { tunModeTouched = true; });
});

function render(status) {
  isConfigured = Boolean(status.configured);
  saveButton.disabled = false;

  document.querySelector("#status-dot").classList.toggle("is-active", status.coreConnected);
  document.querySelector("#status-title").textContent = status.conflict
    ? "发现同名配置，已停止修改"
    : status.coreConnected ? (status.configured ? "Mihomo 正在运行，订阅已启用" : "Mihomo Core 正在运行") : "Mihomo Core 未连接";
  document.querySelector("#status-summary").textContent = status.conflict
    ? "配置文件中已有同名提供者。为保护现有配置，请先在高级面板中处理它。"
    : status.configured
      ? `完整配置每 ${status.intervalHours} 小时同步；其 ${status.providerCount || 0} 个 provider 和策略组按原配置保留。`
      : "保存后会载入完整配置；原有 provider、策略组和规则不会被拆掉。";
  document.querySelector("#proxy-count").textContent = status.configured ? `${status.proxyCount || 0} 个` : "—";
  document.querySelector("#provider-count").textContent = status.configured ? `${status.providerCount || 0} 个` : "—";
  document.querySelector("#last-success").textContent = formatDate(status.lastSuccess);
  document.querySelector("#core-status").textContent = status.coreConnected ? "已连接" : "未连接";
  document.querySelector("#tun-status").textContent = status.tunActive
    ? "Meta 接口运行中"
    : status.tunMode === "on" ? "配置已开，接口未就绪" : "未运行";
  document.querySelector("#proxy-port").textContent = status.proxyPort ? `${status.proxyPort} / TCP+UDP` : "—";
  const providerWarning = document.querySelector("#provider-warning");
  providerWarning.hidden = !status.configured || !status.unreferencedProviderCount;
  providerWarning.textContent = status.unreferencedProviderCount
    ? `有 ${status.unreferencedProviderCount} 个 provider 没有被任何策略组的 use / include-all-providers 引用。按你的要求，管理器保留订阅原样不自动改组；这些节点可在高级面板的 Providers 页面检查，但代理组/规则不会通过它们转发。`
    : "";
  const lastError = document.querySelector("#last-error");
  lastError.hidden = !status.lastError;
  lastError.textContent = status.lastError ? `最近一次自动更新失败：${status.lastError}` : "";
  intervalInput.value = status.intervalHours || 24;
  overlayInput.value = status.overlayYaml || "";
  selectMode("dns-mode", status.dnsMode || "inherit");
  selectMode("tun-mode", status.tunMode || "inherit");
  dnsModeTouched = false;
  tunModeTouched = false;
  urlInput.placeholder = status.configured ? "已保存订阅；留空可保留当前链接" : "https://…";
  document.querySelector("#url-help").textContent = status.configured
    ? "链接保存在 NAS 私有配置中，不会显示在页面或 Mihomo API。留空可保留当前链接；provider 自身按订阅配置的 interval 更新。"
    : "填写返回完整 Clash / Mihomo YAML 的链接。保存时会立即检查；节点列表型 proxies YAML 也可载入。";
  refreshButton.hidden = !status.configured;
  deleteButton.hidden = !status.configured;
  document.querySelector("#version").textContent = `管理页 ${status.version}`;
}

async function loadStatus() {
  try {
    render(await api("api/status"));
  } catch (error) {
    document.querySelector("#status-title").textContent = "无法读取 ShellCrash 状态";
    document.querySelector("#status-summary").textContent = error.message;
    saveButton.disabled = true;
  }
}

async function busy(button, action) {
  const oldText = button.textContent;
  button.disabled = true;
  button.textContent = "处理中…";
  setMessage("");
  try {
    const result = await action();
    if (result.groups) render(result);
    else await loadStatus();
    setMessage("操作完成。");
    return true;
  } catch (error) {
    setMessage(error.message, true);
    return false;
  } finally {
    button.disabled = false;
    button.textContent = oldText;
  }
}

form.addEventListener("submit", (event) => {
  event.preventDefault();
  const intervalHours = Number.parseInt(intervalInput.value, 10);
  if (!Number.isInteger(intervalHours) || intervalHours < 1 || intervalHours > 720) {
    setMessage("自动更新间隔须为 1 到 720 小时。", true);
    return;
  }
  busy(saveButton, () => api("api/subscription", {
    method: "POST",
    body: JSON.stringify({
      url: urlInput.value.trim(),
      intervalHours,
      ...(isConfigured || overlayInput.value.trim() ? { overlayYaml: overlayInput.value } : {}),
      ...(isConfigured || dnsModeTouched ? { dnsMode: selectedMode("dns-mode") } : {}),
      ...(isConfigured || tunModeTouched ? { tunMode: selectedMode("tun-mode") } : {}),
    }),
  })).then((succeeded) => { if (succeeded) urlInput.value = ""; });
});

refreshButton.addEventListener("click", () => {
  busy(refreshButton, () => api("api/refresh", { method: "POST" }));
});

deleteButton.addEventListener("click", () => {
  if (!window.confirm("移除订阅并恢复管理器接管前的 ShellCrash 配置？")) return;
  busy(deleteButton, () => api("api/subscription", { method: "DELETE" }));
});

loadStatus();
