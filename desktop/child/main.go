package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/abemedia/go-webview"
	_ "github.com/abemedia/go-webview/embedded"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/rs/zerolog"

	nappbridge "verdana/backend/webview"
)

type wireMsg struct {
	T      string          `json:"t"`
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params string          `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
	Idx    *int            `json:"idx,omitempty"`
	Record string          `json:"record,omitempty"`
}

type nappMeta struct {
	ID          string
	Name        string
	Description string
	Dir         string
	URL         string
	Instance    string
	Requires    []string
	Theme       string
	ThemeVars   string
}

var (
	meta      nappMeta
	outMu     sync.Mutex
	outEnc    *json.Encoder
	pending   = xsync.NewMapOf[int, chan wireMsg]()
	reqSerial atomic.Int64
	log       zerolog.Logger

	// navViolated is set the moment the window reports it is no longer on
	// the origin the shell picked for it (see the origin guard). From then
	// on the bridge refuses everything this window asks: the document now
	// on screen is not the napp, whatever it looks like.
	navViolated atomic.Bool

	// wRef is the window itself, set once in main before the event loop
	// runs and only read afterwards.
	wRef webview.WebView
)

func main() {
	log = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().
		Int("_", os.Getpid()).
		Timestamp().
		Logger()

	meta = nappMeta{
		ID:          os.Getenv("VERDANA_NAPP_ID"),
		Dir:         os.Getenv("VERDANA_NAPP_DIR"),
		URL:         strings.TrimSpace(os.Getenv("VERDANA_NAPP_URL")),
		Name:        os.Getenv("VERDANA_NAPP_NAME"),
		Description: os.Getenv("VERDANA_NAPP_DESC"),
		Instance:    os.Getenv("VERDANA_INSTANCE_ID"),
		Theme:       os.Getenv("VERDANA_THEME"),
		ThemeVars:   os.Getenv("VERDANA_THEME_VARS"),
	}
	if req := strings.TrimSpace(os.Getenv("VERDANA_NAPP_REQUIRES")); req != "" {
		meta.Requires = strings.Split(req, ",")
	}
	if meta.Name == "" {
		meta.Name = meta.ID
	}
	if meta.Instance == "" {
		meta.Instance = meta.ID
	}

	log.Info().Str("napp", meta.ID).Str("instance", meta.Instance).Msg("napp process started")
	outEnc = json.NewEncoder(os.Stdout)

	runtime.LockOSThread()

	w := webview.New(os.Getenv("WEBVIEW_DEBUG") == "true")
	wRef = w
	w.SetTitle(windowTitle(meta.Name, os.Getenv("VERDANA_INSTANCE_ID")))
	w.SetSize(windowWidth(), windowHeight(), webview.HintNone)
	_ = w.Bind("__bridge_rpc", rpcBound)
	_ = w.Bind("__verdana_nav_violation", navViolation)

	url := meta.URL
	if url == "" {
		url = startNappServer(meta.Dir)
	}
	// The origin guard MUST run FIRST at document start, before storage/theme,
	// UIKit, or bridge.js, so off-origin pages and subframes never receive
	// __nappStorage or host-wide bindings.
	if guard := nappbridge.OriginGuardScript(nappbridge.ExpectedOrigin(url)); guard != "" {
		w.Init(guard)
	}

	// window.name is where bridge.js picks up window.napp.instance, and it
	// survives same-origin navigations — so a reload keeps the instance id.
	// window.__nappTheme is where bridge.js picks the launcher's theme up on
	// every (re)load, so a napp that reloads itself stays in sync.
	// window.__nappStorage seeds the localStorage shim: the file is the
	// per-nappId JSON store the backend persists (native localStorage would
	// be a fresh empty origin on every launch, random port each time).
	w.Init(nappbridge.GateToExpectedOrigin(
		"window.name = " + jsString(meta.Instance) + ";" +
			"window.__nappDomains = " + jsStringSlice(meta.Requires) + ";" +
			storageInitScript(os.Getenv("VERDANA_NAPP_STORAGE_FILE")) +
			themeInitScript(meta.Theme, meta.ThemeVars),
	))
	// the napp-ui kit, for the napps that ask for it with requires: ["ui"]
	if kit := nappbridge.UIKitScript(meta.Requires); kit != "" {
		w.Init(nappbridge.GateToExpectedOrigin(kit))
	}

	// the very same bridge.js the Android app injects
	w.Init(nappbridge.JS())

	w.Navigate(url)

	go reader(w)

	w.Run()
	w.Destroy()
	os.Exit(0)
}

func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// windowWidth/Height are the initial window size in pixels: the napp's
// initial_size via the launcher, falling back to a roomy default. Values are
// clamped like nostrapps sanitizes them (positive, capped at 2000, with a
// minimum that keeps the window usable).
func windowWidth() int { return clampWindowSize(envSize("VERDANA_WINDOW_WIDTH", 1024), 320, 2000) }

func windowHeight() int { return clampWindowSize(envSize("VERDANA_WINDOW_HEIGHT", 700), 240, 2000) }

func envSize(key string, fallback int) int {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return fallback
}

func clampWindowSize(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// windowTitle names the OS window after the napp and its instance, so the
// user can match it to the "window <instance>" entries in action prompts.
func windowTitle(name, instance string) string {
	if name == "" {
		name = "Napp"
	}
	if instance == "" {
		return name
	}
	return name + " - window " + instance
}

// storageInitScript seeds window.__nappStorage, which the bridge's
// localStorage shim runs synchronously from. The file is the backend's
// per-nappId JSON store; missing/corrupt means start empty.
func storageInitScript(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return "window.__nappStorage = {};"
	}
	var probe map[string]string
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "window.__nappStorage = {};"
	}
	return "window.__nappStorage = " + string(raw) + ";"
}

// themeInitScript sets window.__nappTheme, which bridge.js applies as soon as
// it runs. varsJSON comes from the launcher, so it is already valid JSON.
func themeInitScript(name, varsJSON string) string {
	if name == "" {
		name = "light"
	}
	if strings.TrimSpace(varsJSON) == "" {
		varsJSON = "{}"
	}
	return "window.__nappTheme = {name:" + jsString(name) + ",vars:" + varsJSON + "};"
}

func jsStringSlice(items []string) string {
	b, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func startNappServer(root string) string {
	if root == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		log.Debug().Str("root", root).Msg("no index.html found for napp")
		return ""
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Error().Err(err).Str("root", root).Msg("failed to listen for napp server")
		return ""
	}
	// addr is what this server answers on; requests carrying a different
	// Host are someone other than the webview — a DNS-rebinding page (an
	// outside origin pointed at 127.0.0.1 to read the napp's files) names
	// its own host in the Host header, and gets nothing.
	addr := ln.Addr().String()
	fs := http.FileServer(http.Dir(root))
	handler := http.HandlerFunc(func(wr http.ResponseWriter, r *http.Request) {
		// Top-level navigation is kept inside this napp origin by the
		// origin guard injected at document start (see OriginGuardScript):
		// "navigate-to" would say it in CSP, but no browser ships it.
		if r.Host != addr {
			http.Error(wr, "forbidden", http.StatusForbidden)
			return
		}
		clean := filepath.Join(root, filepath.FromSlash(path.Clean("/"+r.URL.Path)))
		if st, statErr := os.Stat(clean); statErr != nil || st.IsDir() {
			if r.URL.Path != "/" && !strings.Contains(path.Base(r.URL.Path), ".") {
				http.ServeFile(wr, r, filepath.Join(root, "index.html"))
				return
			}
		}
		fs.ServeHTTP(wr, r)
	})
	go http.Serve(ln, handler)
	return "http://" + ln.Addr().String() + "/"
}

func rpcBound(method string, params string) string {
	if navViolated.Load() {
		return `{"__bridge_error":"this window left the napp origin; the bridge is off"}`
	}
	result, err := rpc(method, params)
	if err != nil {
		wrapped, _ := json.Marshal(map[string]string{"__bridge_error": err.Error()})
		return string(wrapped)
	}
	if len(result) == 0 {
		return "null"
	}
	return string(result)
}

func rpc(method string, params string) (json.RawMessage, error) {
	id := int(reqSerial.Add(1))
	ch := make(chan wireMsg, 1)
	pending.Store(id, ch)

	writeMsg(wireMsg{T: "rpc", ID: id, Method: method, Params: params})

	resp := <-ch
	pending.Delete(id)

	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}

func reader(w webview.WebView) {
	dec := json.NewDecoder(os.Stdin)
	for {
		var m wireMsg
		if err := dec.Decode(&m); err != nil {
			break
		}
		switch m.T {
		case "resp":
			if ch, ok := pending.Load(m.ID); ok {
				ch <- m
			}
		case "eval":
			code := m.Code
			w.Dispatch(func() { w.Eval(code) })
		case "action":
			// the launcher routed an action here: hand it to the napp's
			// registered handler (idx) and/or its popstate listener
			idx := "null"
			if m.Idx != nil {
				idx = strconv.Itoa(*m.Idx)
			}
			payload := m.Params
			if payload == "" {
				payload = "null"
			}
			code := "window.__bridge_dispatch_action(" +
				strconv.Itoa(m.ID) + "," + jsString(m.Method) + "," + jsString(payload) + "," + idx + "," +
				jsString(m.Record) + ")"
			w.Dispatch(func() { w.Eval(code) })
		case "theme":
			// the launcher switched theme: apply it now, and make it the value
			// future page loads in this window start from
			init := themeInitScript(m.Method, m.Params)
			vars := m.Params
			if vars == "" {
				vars = "{}"
			}
			code := init +
				"if (window.__bridge_theme_change) window.__bridge_theme_change(" +
				jsString(m.Method) + "," + jsString(vars) + ");"
			w.Dispatch(func() {
				w.Init(init)
				w.Eval(code)
			})
		case "close":
			w.Dispatch(func() { w.Terminate() })
		}
	}
	w.Terminate()
}

func writeMsg(m wireMsg) {
	outMu.Lock()
	defer outMu.Unlock()
	outEnc.Encode(m)
}

// navViolation is what the origin guard calls when the main frame has landed
// somewhere other than the napp's own origin. go-webview runs bound callbacks
// asynchronously in a goroutine (the guard script synchronously deletes the
// JS bindings first); this permanently disables host-side RPCs, reports the
// violation once, and unbinds the native bindings.
func navViolation() string {
	if navViolated.Swap(true) {
		return ""
	}
	writeMsg(wireMsg{T: "navViolation"})
	if wRef != nil {
		wRef.Dispatch(func() {
			_ = wRef.Unbind("__bridge_rpc")
			_ = wRef.Unbind("__verdana_nav_violation")
		})
	}
	log.Warn().Str("napp", meta.ID).Str("instance", meta.Instance).
		Msg("window navigated off the napp origin; bridge disabled")
	return "ok"
}
