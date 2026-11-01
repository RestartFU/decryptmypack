const RECENT_TARGETS_KEY = "decryptmypack_recent_targets";
const MAX_RECENT_TARGETS = 4;
const POLL_INTERVAL_MS = 3000;
const DOWNLOAD_TIMEOUT_MS = 5 * 60 * 1000;

let downloadInFlight = false;
let cancelDownloadRequested = false;
let retryDownloadTarget = "";
let lastDownloadURL = "";
let browseServers = [];
let selectedBrowseTarget = "";
let browseSearchTimer = null;
let browseAbortController = null;
let browseRequestID = 0;
let statsTimer = null;
let elapsedTimer = null;
let elapsedStartedAt = 0;
let elapsedTarget = "";
let errorTimer = null;
let discordTimer = null;
let currentStage = -1;
let accounts = [];
let serviceAccount = true;
let loginID = "";
let loginTimer = null;
let toastTimer = null;

const API_HEADERS = { "X-DMP-Request": "1" };
const LOGIN_POLL_MS = 3000;

const STAGE_COUNT = 5;
const STAGE_READY = 4;

function storageGet(key) {
    try {
        return window.localStorage?.getItem(key) || "";
    } catch {
        return "";
    }
}

function storageSet(key, value) {
    try {
        window.localStorage?.setItem(key, value);
    } catch {
        // Recent targets are optional when browser storage is unavailable.
    }
}

function loadRecentTargets() {
    try {
        const parsed = JSON.parse(storageGet(RECENT_TARGETS_KEY) || "[]");
        return Array.isArray(parsed) ? parsed.filter(Boolean).slice(0, MAX_RECENT_TARGETS) : [];
    } catch {
        return [];
    }
}

function saveRecentTarget(target) {
    const clean = target.trim();
    if (!clean) return;
    const targets = loadRecentTargets().filter((item) => item.toLowerCase() !== clean.toLowerCase());
    targets.unshift(clean);
    storageSet(RECENT_TARGETS_KEY, JSON.stringify(targets.slice(0, MAX_RECENT_TARGETS)));
    renderRecentTargets();
}

function renderRecentTargets() {
    const container = document.getElementById("recent-targets");
    if (!container) return;
    const targets = loadRecentTargets();
    container.replaceChildren();
    container.hidden = targets.length === 0;
    if (!targets.length) return;

    const label = document.createElement("span");
    label.className = "recent-label";
    label.textContent = "Recent";
    container.appendChild(label);
    for (const target of targets) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "recent-target";
        button.textContent = target;
        button.addEventListener("click", () => downloadSpecifiedServer(target));
        container.appendChild(button);
    }
}

function updateInputValue(value) {
    const input = document.getElementById("input");
    if (input) input.value = value;
}

function downloadSpecifiedServer(address) {
    updateInputValue(address);
    decrypt();
}

function selectServer(address) {
    selectedBrowseTarget = address;
    updateInputValue(address);
    renderBrowseServers();
    decrypt();
}

function setBusy(busy) {
    const button = document.getElementById("decrypt-btn");
    const input = document.getElementById("input");
    if (button) {
        button.disabled = busy;
        button.textContent = busy ? "Decrypting…" : "Decrypt pack";
    }
    if (input) input.disabled = busy;
    document.querySelectorAll(".server-cell").forEach((element) => {
        element.disabled = busy;
    });
}

function stopElapsedTimer() {
    window.clearInterval(elapsedTimer);
    elapsedTimer = null;
    elapsedStartedAt = 0;
    elapsedTarget = "";
}

function elapsedText() {
    const seconds = Math.max(0, Math.floor((Date.now() - elapsedStartedAt) / 1000));
    const minutes = Math.floor(seconds / 60);
    const remainder = seconds % 60;
    return minutes ? `${minutes}m ${String(remainder).padStart(2, "0")}s` : `${seconds}s`;
}

function startElapsedTimer(target) {
    if (!elapsedStartedAt || elapsedTarget !== target) {
        elapsedStartedAt = Date.now();
        elapsedTarget = target;
    }
    const render = () => setStatus(
        "loading",
        "Connecting",
        `Connecting to server… ${elapsedText()} elapsed`,
        target,
        { keepTimer: true, showWait: true, stage: 1 },
    );
    render();
    if (!elapsedTimer) elapsedTimer = window.setInterval(render, 1000);
}

// buildStageTiles fills every stage tile with the 4×4 cells its dissolve animates.
function buildStageTiles() {
    document.querySelectorAll(".stage-tile").forEach((tile) => {
        if (tile.childElementCount) return;
        for (let index = 0; index < 16; index += 1) tile.appendChild(document.createElement("i"));
    });
}

// stageForMessage maps a backend progress message onto the five visible stages.
function stageForMessage(message) {
    const text = String(message || "").toLowerCase();
    if (text.includes("downloading pack")) return 2;
    if (text.includes("encoding") || text.includes("decrypting")) return 3;
    if (text.includes("saving") || text === "ready") return STAGE_READY;
    return 1;
}

// renderStages paints each stage tile as done, active, failed, or pending.
function renderStages(state) {
    const stages = document.querySelectorAll("#status-stages .stage");
    stages.forEach((stage, index) => {
        let stageState = "pending";
        if (state === "success") stageState = "done";
        else if (index < currentStage) stageState = "done";
        else if (index === currentStage) stageState = state === "error" ? "failed" : state === "loading" ? "active" : "pending";
        stage.dataset.stageState = stageState;
    });
}

// markCacheHit flags the Cache stage so a pack served straight from the cache fills diamond.
function markCacheHit(hit) {
    const first = document.querySelector("#status-stages .stage");
    if (first) first.dataset.cacheHit = String(hit);
}

function setStatus(state, title, text, target = "", options = {}) {
    if (!options.keepTimer) stopElapsedTimer();
    if (typeof options.stage === "number") currentStage = Math.min(Math.max(options.stage, currentStage), STAGE_COUNT - 1);
    if (options.resetStages) currentStage = options.stage ?? -1;
    renderStages(state);
    const card = document.getElementById("status-card");
    const dot = document.getElementById("status-dot");
    if (card) card.dataset.state = state;
    if (dot) dot.dataset.state = state;
    document.getElementById("status-title").textContent = title;
    document.getElementById("status-text").textContent = text;
    document.getElementById("status-target").textContent = target;
    document.getElementById("status-actions").hidden = !options.showDownload;
    document.getElementById("status-wait-actions").hidden = !options.showWait;
    document.getElementById("status-retry-actions").hidden = !options.showRetry;
}

function showError(message) {
    const element = document.getElementById("error");
    if (!element) return;
    window.clearTimeout(errorTimer);
    element.textContent = message;
    element.dataset.visible = "true";
    errorTimer = window.setTimeout(hideError, 15000);
}

function hideError() {
    const element = document.getElementById("error");
    if (!element) return;
    window.clearTimeout(errorTimer);
    element.dataset.visible = "false";
}

function parseTarget(input) {
    let target = input.trim();
    if (!target) return { host: "", port: "19132" };
    try {
        if (/^[a-z][a-z0-9+.-]*:\/\//i.test(target)) {
            const parsed = new URL(target);
            const external = parsed.searchParams.get("addExternalServer");
            if (external) {
                target = safeDecode(external).split("|").pop().trim();
            } else if (parsed.hostname) {
                return { host: parsed.hostname.trim(), port: parsed.port || "19132" };
            }
        }
    } catch {
        // Fall through to plain host parsing.
    }
    target = target.replace(/^bedrock:\/\//i, "").replace(/^minecraft:\/\//i, "");
    const bracketed = target.match(/^\[([^\]]+)](?::(\d+))?$/);
    if (bracketed) return { host: bracketed[1].trim(), port: bracketed[2] || "19132" };
    if ((target.match(/:/g) || []).length === 1) {
        const [host, port] = target.split(":");
        return { host: host.trim(), port: (port || "19132").trim() };
    }
    return { host: target, port: "19132" };
}

function safeDecode(value) {
    try {
        return decodeURIComponent(value);
    } catch {
        return value;
    }
}

function formatTarget(target) {
    if (!target.host) return "";
    const host = target.host.includes(":") && !target.host.startsWith("[") ? `[${target.host}]` : target.host;
    return target.port === "19132" ? host : `${host}:${target.port}`;
}

function targetURLs(target) {
    const download = new URL("/download", window.location.origin);
    const status = new URL("/status", window.location.origin);
    for (const url of [download, status]) {
        url.searchParams.set("host", target.host);
        url.searchParams.set("port", target.port);
    }
    return { download, status };
}

async function decrypt() {
    if (downloadInFlight) return;
    const input = document.getElementById("input");
    let raw = input.value.trim();
    if (!raw) {
        raw = input.placeholder.trim();
        input.value = raw;
    }
    const target = parseTarget(raw);
    if (!target.host || !/^\d+$/.test(target.port) || Number(target.port) < 1 || Number(target.port) > 65535) {
        const message = "Invalid server address. Use host or host:port.";
        showError(message);
        setStatus("error", "Download failed", message, `Target: ${raw}`, { showRetry: true, resetStages: true, stage: -1 });
        return;
    }

    const displayTarget = formatTarget(target);
    const account = activeAccount();
    const targetLine = account ? `Target: ${formatTarget(target)} · as ${account.name}` : `Target: ${formatTarget(target)}`;
    input.value = displayTarget;
    retryDownloadTarget = displayTarget;
    downloadInFlight = true;
    cancelDownloadRequested = false;
    setBusy(true);
    hideError();
    markCacheHit(false);
    setStatus("loading", "Checking cache", "Looking for an existing pack.", targetLine, { resetStages: true, stage: 0 });

    try {
        const urls = targetURLs(target);
        let response = await fetch(urls.download, { headers: downloadHeaders() });
        let body = (await response.text()).trim();
        if (!response.ok) throw new Error(body || `Request failed with status ${response.status}`);

        const cacheHit = !(response.status === httpAccepted() || body === "processing");
        markCacheHit(cacheHit);
        if (!cacheHit) {
            setStatus(
                "loading",
                "Generating pack",
                "Connecting, downloading, and decrypting the resource pack.",
                targetLine,
                { showWait: true, stage: 1 },
            );
            body = await pollForPack(urls.download, urls.status, targetLine);
        }

        const safeURL = safeDownloadURL(body);
        lastDownloadURL = safeURL;
        saveRecentTarget(displayTarget);
        setStatus("success", "Pack ready", "Your download is starting.", cacheHit ? `Target: ${displayTarget}` : targetLine, { showDownload: true, stage: STAGE_READY });
        triggerDownload(safeURL);
        scheduleDiscordPopup();
        loadStats();
        loadBrowseServers();
    } catch (error) {
        if (error.cancelled) {
            setStatus("idle", "Stopped waiting", "The server may finish this pack in the background.", targetLine, { showRetry: true, resetStages: true, stage: -1 });
        } else {
            const message = error.message || "Download failed";
            showError(`Error downloading file: ${message}`);
            setStatus("error", "Download failed", message, targetLine, { showRetry: true });
        }
    } finally {
        setBusy(false);
        downloadInFlight = false;
    }
}

function httpAccepted() {
    return 202;
}

async function pollForPack(downloadURL, statusURL, targetLine) {
    const deadline = Date.now() + DOWNLOAD_TIMEOUT_MS;
    while (Date.now() < deadline) {
        if (cancelDownloadRequested) {
            const error = new Error("Stopped waiting");
            error.cancelled = true;
            throw error;
        }
        await delay(POLL_INTERVAL_MS);
        const response = await fetch(downloadURL, { headers: downloadHeaders() });
        const body = (await response.text()).trim();
        if (!response.ok) throw new Error(body || `Request failed with status ${response.status}`);
        if (response.status !== httpAccepted() && body !== "processing") {
            if (!body) throw new Error("Unexpected empty download response");
            return body;
        }
        await updateProcessingStatus(statusURL, targetLine);
    }
    throw new Error("Timed out waiting for the pack");
}

async function updateProcessingStatus(statusURL, targetLine) {
    try {
        const response = await fetch(statusURL);
        if (!response.ok) throw new Error("status unavailable");
        const payload = await response.json();
        const message = payload.status || "processing";
        if (message === "failed" || message === "error") {
            throw new Error(payload.error || "The download attempt failed.");
        }
        if (message.toLowerCase() === "connecting to server") {
            startElapsedTimer(targetLine);
            return;
        }
        const stage = stageForMessage(message);
        const title = message === "ready" ? "Almost ready" : ["Checking cache", "Joining server", "Downloading packs", "Decrypting packs", "Saving pack"][stage];
        setStatus("loading", title, message === "ready" ? "Preparing your download." : sentenceCase(message), targetLine, { showWait: true, stage });
    } catch (error) {
        if (error.message !== "status unavailable") throw error;
        setStatus("loading", "Still working", "The server is still processing the pack.", targetLine, { showWait: true, stage: 1 });
    }
}

function sentenceCase(text) {
    const value = String(text || "");
    return value ? value.charAt(0).toUpperCase() + value.slice(1) : value;
}

function safeDownloadURL(value) {
    let parsed;
    try {
        parsed = new URL(String(value || "").trim(), window.location.href);
    } catch {
        throw new Error("Unexpected invalid download response");
    }
    if (!["http:", "https:"].includes(parsed.protocol)) throw new Error("Unexpected unsafe download response");
    return parsed.toString();
}

function triggerDownload(url) {
    const anchor = document.createElement("a");
    anchor.href = safeDownloadURL(url);
    anchor.rel = "noopener noreferrer";
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
}

function delay(milliseconds) {
    return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

function cancelDownloadWait() {
    if (downloadInFlight) cancelDownloadRequested = true;
}

function retryLastDownload() {
    if (retryDownloadTarget && !downloadInFlight) downloadSpecifiedServer(retryDownloadTarget);
}

function downloadAgain() {
    if (lastDownloadURL) triggerDownload(lastDownloadURL);
}

function formatCount(value) {
    const numeric = Number(value || 0);
    return new Intl.NumberFormat("en-US", {
        notation: numeric >= 10000 ? "compact" : "standard",
        maximumFractionDigits: 1,
    }).format(numeric);
}

async function loadStats() {
    try {
        const response = await fetch("/stats");
        if (!response.ok) throw new Error("stats unavailable");
        const stats = await response.json();
        document.getElementById("total-downloads").textContent = formatCount(stats.successful_downloads);
        document.getElementById("unique-downloads").textContent = formatCount(stats.unique_successful_targets);
    } catch {
        document.getElementById("total-downloads").textContent = "--";
        document.getElementById("unique-downloads").textContent = "--";
    }
}

async function loadBrowseServers() {
    const requestID = ++browseRequestID;
    browseAbortController?.abort();
    browseAbortController = new AbortController();
    try {
        const query = document.getElementById("browse-search").value.trim();
        const url = new URL("/browse", window.location.origin);
        if (query) url.searchParams.set("q", query);
        const response = await fetch(url, { signal: browseAbortController.signal });
        if (!response.ok) throw new Error("browse unavailable");
        const payload = await response.json();
        if (requestID !== browseRequestID) return;
        const featured = (payload.featured || []).map((server) => ({ ...server, featured: true }));
        const seen = new Set(featured.map(serverTarget));
        const popular = (payload.popular || []).filter((server) => {
            const target = serverTarget(server);
            if (seen.has(target)) return false;
            seen.add(target);
            return true;
        });
        browseServers = [...featured, ...popular];
        renderBrowseServers();
    } catch (error) {
        if (error.name !== "AbortError" && !browseServers.length) renderBrowseMessage("Could not load servers.");
    }
}

function visibleBrowseServers() {
    const query = document.getElementById("browse-search").value.trim().toLowerCase();
    return browseServers
        .filter((server) => !query || serverLabel(server).toLowerCase().includes(query))
        .sort((a, b) => Number(b.downloads || 0) - Number(a.downloads || 0))
        .slice(0, query ? 24 : 12);
}

function renderBrowseServers() {
    const list = document.getElementById("popular-server-list");
    const query = document.getElementById("browse-search").value.trim();
    const servers = visibleBrowseServers();
    if (!servers.length) {
        renderBrowseMessage(query ? `No matches for "${query}". You can still enter it above.` : "No servers are available yet.");
        return;
    }
    list.replaceChildren();
    for (const server of servers) {
        const target = serverTarget(server);
        const button = document.createElement("button");
        button.type = "button";
        button.className = "server-cell";
        button.disabled = downloadInFlight;
        const selected = target === selectedBrowseTarget;
        button.classList.toggle("is-selected", selected);
        button.setAttribute("aria-pressed", String(selected));
        button.addEventListener("click", () => selectServer(target));

        const icon = server.icon ? document.createElement("img") : fallbackIcon(server);
        if (server.icon) {
            icon.className = "server-icon";
            icon.src = server.icon;
            icon.alt = "";
            icon.loading = "lazy";
            icon.width = 48;
            icon.height = 48;
            icon.addEventListener("error", () => icon.replaceWith(fallbackIcon(server)));
        }

        const copy = document.createElement("span");
        copy.className = "server-copy";
        const name = document.createElement("span");
        name.className = "server-name";
        name.textContent = server.name || target;
        const address = document.createElement("span");
        address.className = "server-address";
        address.textContent = target;
        const meta = document.createElement("span");
        meta.className = "server-meta";
        if (["cached", "uncached", "running", "failed"].includes(server.cache_state)) {
            const glyph = document.createElement("i");
            glyph.className = `cache-glyph cache-glyph-${server.cache_state}`;
            glyph.setAttribute("aria-hidden", "true");
            meta.appendChild(glyph);
        }
        const count = Number(server.downloads || 0);
        const countText = count ? `${formatCount(count)} ${count === 1 ? "download" : "downloads"}` : server.featured ? "Featured" : "No downloads yet";
        meta.append(`${countText}${CACHE_LABELS[server.cache_state] ? ` · ${CACHE_LABELS[server.cache_state]}` : ""}`);
        copy.append(name);
        if (server.name) copy.append(address);
        copy.append(meta);
        button.append(icon, copy);
        list.appendChild(button);
    }
}

function renderBrowseMessage(message) {
    const list = document.getElementById("popular-server-list");
    const empty = document.createElement("div");
    empty.className = "popular-empty";
    empty.textContent = message;
    list.replaceChildren(empty);
}

function serverTarget(server) {
    return server.target || (String(server.port || "19132") === "19132" ? server.host : `${server.host}:${server.port}`);
}

function serverLabel(server) {
    return server.name ? `${server.name} · ${serverTarget(server)}` : serverTarget(server);
}

const CACHE_LABELS = {
    cached: "cached",
    running: "downloading now",
    uncached: "not cached yet",
    failed: "last attempt failed",
};

// fallbackIcon draws a lettered block tile; the block colour is stable per server name.
function fallbackIcon(server) {
    const label = server.name || server.host || "?";
    const icon = document.createElement("span");
    icon.className = "server-icon server-fallback";
    icon.setAttribute("aria-hidden", "true");
    icon.dataset.block = String([...label].reduce((sum, character) => sum + character.charCodeAt(0), 0) % 6);
    icon.textContent = label.slice(0, 1).toUpperCase();
    return icon;
}

function queueBrowseSearch() {
    renderBrowseServers();
    window.clearTimeout(browseSearchTimer);
    browseSearchTimer = window.setTimeout(loadBrowseServers, 300);
}

function downloadFirstBrowseMatch() {
    const match = visibleBrowseServers()[0];
    if (!match) return false;
    selectServer(serverTarget(match));
    return true;
}

// downloadHeaders names the selected account so an uncached download joins the server with it.
function downloadHeaders() {
    const account = activeAccount();
    return account ? { "X-DMP-Account": account.id } : {};
}

function activeAccount() {
    return accounts.find((account) => account.active) || null;
}

// accountAPI calls the same-origin account endpoints and turns error bodies into readable errors.
async function accountAPI(method, path) {
    const response = await fetch(path, { method, headers: API_HEADERS, credentials: "same-origin" });
    const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.error || `Request failed with status ${response.status}`);
    return body;
}

async function loadAccounts() {
    try {
        const body = await accountAPI("GET", "/api/accounts");
        accounts = body.accounts || [];
        serviceAccount = body.serviceAccount !== false;
    } catch {
        accounts = [];
    }
    renderAccounts();
}

function initials(name) {
    return name.trim().split(/\s+/).slice(0, 2).map((part) => part[0] || "").join("") || "?";
}

function renderAccounts() {
    const list = document.getElementById("account-list");
    if (!list) return;
    list.replaceChildren();
    const note = document.getElementById("rail-note");
    if (note) {
        note.textContent = serviceAccount
            ? "New servers are joined with your selected account, or the site's shared account if you have none. Every finished pack goes into the shared cache."
            : "Servers that aren't cached yet are joined with your selected account. Every finished pack goes into the shared cache.";
    }
    if (!accounts.length) {
        const empty = document.createElement("p");
        empty.className = "empty-accounts";
        empty.textContent = serviceAccount
            ? "No account needed for cached servers. Add your Microsoft account to fetch new ones with it."
            : "Cached servers need no account. To fetch a server that isn't cached yet, add your Microsoft account.";
        list.appendChild(empty);
        return;
    }
    for (const account of accounts) {
        const row = document.createElement("div");
        row.className = account.active ? "account-row active" : "account-row";
        row.tabIndex = 0;
        row.setAttribute("role", "button");
        row.setAttribute("aria-pressed", String(account.active));
        const avatar = document.createElement("span");
        avatar.className = "avatar";
        avatar.textContent = initials(account.name);
        const name = document.createElement("span");
        name.className = "account-name";
        name.textContent = account.name;
        const remove = document.createElement("button");
        remove.type = "button";
        remove.className = "remove-account";
        remove.setAttribute("aria-label", `Remove ${account.name}`);
        remove.appendChild(closeIcon());
        remove.addEventListener("click", (event) => {
            event.stopPropagation();
            removeAccount(account);
        });
        row.append(avatar, name, remove);
        row.addEventListener("click", () => selectAccount(account));
        row.addEventListener("keydown", (event) => {
            if (event.target === row && (event.key === "Enter" || event.key === " ")) {
                event.preventDefault();
                selectAccount(account);
            }
        });
        list.appendChild(row);
    }
}

// closeIcon draws the shared pixel cross used for remove and close buttons.
function closeIcon() {
    const namespace = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(namespace, "svg");
    svg.setAttribute("class", "icon");
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("aria-hidden", "true");
    const path = document.createElementNS(namespace, "path");
    path.setAttribute("d", "M3 3h2v2H3zM5 5h2v2H5zM7 7h2v2H7zM9 9h2v2H9zM11 11h2v2h-2zM11 3h2v2h-2zM9 5h2v2H9zM5 9h2v2H5zM3 11h2v2H3z");
    svg.appendChild(path);
    return svg;
}

async function selectAccount(account) {
    if (account.active || downloadInFlight) return;
    try {
        accounts = (await accountAPI("POST", `/api/accounts/${encodeURIComponent(account.id)}/select`)).accounts || [];
        renderAccounts();
    } catch (error) {
        toast(error.message);
    }
}

async function removeAccount(account) {
    if (!window.confirm(`Remove ${account.name} from this browser?`)) return;
    try {
        accounts = (await accountAPI("DELETE", `/api/accounts/${encodeURIComponent(account.id)}`)).accounts || [];
        renderAccounts();
        toast(`${account.name} removed.`);
    } catch (error) {
        toast(error.message);
    }
}

function openAccountDialog() {
    const modal = document.getElementById("account-modal");
    document.getElementById("login-step").hidden = false;
    document.getElementById("code-step").hidden = true;
    openModal(modal);
}

async function closeAccountDialog() {
    const modal = document.getElementById("account-modal");
    if (modal) modal.hidden = true;
    window.clearTimeout(loginTimer);
    loginTimer = null;
    const pending = loginID;
    loginID = "";
    if (pending) {
        try {
            await accountAPI("DELETE", `/api/accounts/login/${encodeURIComponent(pending)}`);
        } catch {
            // The sign-in may already have finished or expired.
        }
    }
    document.getElementById("add-account")?.focus();
}

async function beginLogin() {
    const button = document.getElementById("begin-login");
    button.disabled = true;
    button.textContent = "Starting…";
    try {
        const challenge = await accountAPI("POST", "/api/accounts/login");
        loginID = challenge.loginId;
        document.getElementById("login-code").textContent = challenge.userCode;
        document.getElementById("login-status").textContent = "Waiting for approval…";
        const link = document.getElementById("open-login");
        link.href = safeExternalURL(challenge.verificationUriComplete || challenge.verificationUri);
        document.getElementById("login-step").hidden = true;
        document.getElementById("code-step").hidden = false;
        link.focus();
        loginTimer = window.setTimeout(pollLogin, LOGIN_POLL_MS);
    } catch (error) {
        toast(error.message);
    } finally {
        button.disabled = false;
        button.textContent = "Continue with Microsoft";
    }
}

async function pollLogin() {
    if (!loginID) return;
    try {
        const state = await accountAPI("GET", `/api/accounts/login/${encodeURIComponent(loginID)}`);
        if (state.state === "done") {
            loginID = "";
            await closeAccountDialog();
            await loadAccounts();
            toast(`${state.account?.name || "Account"} is ready.`);
            return;
        }
        if (state.state === "failed") {
            loginID = "";
            document.getElementById("login-status").textContent = state.error || "Microsoft sign-in did not finish.";
            return;
        }
    } catch (error) {
        loginID = "";
        document.getElementById("login-status").textContent = error.message;
        return;
    }
    loginTimer = window.setTimeout(pollLogin, LOGIN_POLL_MS);
}

// safeExternalURL only lets Microsoft's https sign-in pages through to the link.
function safeExternalURL(value) {
    try {
        const url = new URL(value);
        if (url.protocol === "https:" && /(^|\.)(microsoft|live)\.com$/i.test(url.hostname)) return url.toString();
    } catch {
        // Fall back to the public device-code page.
    }
    return "https://www.microsoft.com/link";
}

async function copyLoginCode() {
    const code = document.getElementById("login-code").textContent.trim();
    if (!loginID || !code) return;
    try {
        await navigator.clipboard.writeText(code);
        toast("Code copied.");
    } catch {
        toast("Select the code and copy it manually.");
    }
}

function toast(message) {
    const element = document.getElementById("toast");
    if (!element) return;
    element.textContent = message;
    element.hidden = false;
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(() => {
        element.hidden = true;
    }, 3500);
}

function cookieValue(name) {
    return document.cookie.split(";").map((item) => item.trim()).find((item) => item.startsWith(`${name}=`)) || "";
}

function scheduleDiscordPopup() {
    if (cookieValue("decryptmypack_discord_popup_seen") || discordTimer) return;
    discordTimer = window.setTimeout(() => {
        document.cookie = "decryptmypack_discord_popup_seen=1; Max-Age=31536000; Path=/; SameSite=Lax";
        openModal(document.getElementById("discord-modal"));
    }, 15000);
}

function openModal(modal) {
    if (!modal) return;
    modal.hidden = false;
    window.setTimeout(() => modal.querySelector("button")?.focus(), 0);
}

function closeDiscordModal() {
    const modal = document.getElementById("discord-modal");
    if (modal) modal.hidden = true;
}

function toDiscord() {
    window.location.href = "https://discord.gg/YDaxHMySd7";
}

function initialise() {
    document.getElementById("decrypt-btn")?.addEventListener("click", decrypt);
    document.getElementById("download-again")?.addEventListener("click", downloadAgain);
    document.getElementById("cancel-download-wait")?.addEventListener("click", cancelDownloadWait);
    document.getElementById("retry-download")?.addEventListener("click", retryLastDownload);
    document.getElementById("discord-btn")?.addEventListener("click", toDiscord);
    document.getElementById("discord-dismiss")?.addEventListener("click", closeDiscordModal);
    document.getElementById("discord-join")?.addEventListener("click", () => {
        closeDiscordModal();
        toDiscord();
    });
    document.getElementById("input")?.addEventListener("keydown", (event) => {
        if (event.key === "Enter" && !event.isComposing) {
            event.preventDefault();
            decrypt();
        }
    });
    document.getElementById("browse-search")?.addEventListener("input", queueBrowseSearch);
    document.getElementById("browse-search")?.addEventListener("keydown", (event) => {
        if (event.key === "Enter" && !event.isComposing) {
            event.preventDefault();
            if (!downloadFirstBrowseMatch()) downloadSpecifiedServer(event.currentTarget.value);
        }
    });
    document.getElementById("add-account")?.addEventListener("click", openAccountDialog);
    document.getElementById("account-modal-close")?.addEventListener("click", closeAccountDialog);
    document.getElementById("account-modal")?.addEventListener("click", (event) => {
        if (event.target.id === "account-modal") closeAccountDialog();
    });
    document.getElementById("begin-login")?.addEventListener("click", beginLogin);
    document.getElementById("copy-code")?.addEventListener("click", copyLoginCode);
    document.addEventListener("keydown", (event) => {
        if (event.key !== "Escape") return;
        closeDiscordModal();
        if (!document.getElementById("account-modal")?.hidden) closeAccountDialog();
    });

    buildStageTiles();
    loadAccounts();
    renderRecentTargets();
    setStatus("idle", "Ready when you are", "Enter a server and start a download.");
    loadStats();
    loadBrowseServers();
    statsTimer = window.setInterval(() => {
        if (document.visibilityState === "visible") {
            loadStats();
            loadBrowseServers();
        }
    }, 15000);
}

document.addEventListener("DOMContentLoaded", initialise);
