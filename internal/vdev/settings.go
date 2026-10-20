package vdev

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"hdhrmux/internal/config"
)

// The settings page's API. It lets people change the configuration without
// editing JSON: values are stored by config.Manager in settings.json in the
// state directory. Because it can reconfigure the multiplexer it is limited to
// clients on private networks, needs a custom request header (which a web page
// on another site cannot send without our consent) and can be locked.

const adminHeader = "X-Requested-With"
const adminHeaderValue = "hdhr-mux-ui"

func (d *Device) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/api/settings", d.handleSettingsGet)
	mux.HandleFunc("PUT /ui/api/settings", d.handleSettingsPut)
	mux.HandleFunc("POST /ui/api/restart", d.handleRestart)
}

// adminOK checks that the request may read or change settings, and says why not.
func (d *Device) adminOK(w http.ResponseWriter, r *http.Request, write bool) bool {
	if d.cfg.Settings == nil {
		http.Error(w, "settings are not available in this setup", http.StatusNotFound)
		return false
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		http.Error(w, "settings can only be used from the local network", http.StatusForbidden)
		return false
	}
	if write {
		if d.cfg.SettingsLocked {
			http.Error(w, "settings are locked (settings_locked in the config file)", http.StatusForbidden)
			return false
		}
		if r.Header.Get(adminHeader) != adminHeaderValue {
			http.Error(w, "missing "+adminHeader+" header", http.StatusForbidden)
			return false
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return false
			}
		}
	}
	return true
}

func (d *Device) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !d.adminOK(w, r, false) {
		return
	}
	m := d.cfg.Settings
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"schema":         config.Schema(),
		"values":         m.Values(),
		"saved":          m.Saved(),
		"env":            m.OverriddenByEnv(),
		"storage":        config.StorageOf(d.cfg.StateDir),
		"configPath":     m.ConfigPath(),
		"settingsPath":   m.FilePath(),
		"locked":         d.cfg.SettingsLocked,
		"restartable":    d.cfg.Restart != nil,
		"pendingRestart": m.PendingRestart(),
		"setup":          d.cfg.Setup,
	})
}

func (d *Device) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !d.adminOK(w, r, true) {
		return
	}
	var body struct {
		Changes map[string]any `json:"changes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	if err := d.cfg.Settings.Save(body.Changes); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// Apply what can be applied now; the rest waits for a restart.
	c, err := d.cfg.Settings.Reload()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "saved, but reading it back failed: " + err.Error()})
		return
	}
	d.ApplyConfig(c)
	log.Printf("settings: saved %d change(s) from %s", len(body.Changes), r.RemoteAddr)
	writeJSON(w, map[string]any{"ok": true, "pendingRestart": d.cfg.Settings.PendingRestart(), "values": d.cfg.Settings.Values(), "saved": d.cfg.Settings.Saved()})
}

func (d *Device) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !d.adminOK(w, r, true) {
		return
	}
	if d.cfg.Restart == nil {
		http.Error(w, "this setup cannot restart itself", http.StatusNotImplemented)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(300 * time.Millisecond) // let the answer reach the browser
		d.cfg.Restart()
	}()
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
