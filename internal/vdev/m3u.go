package vdev

import (
	"fmt"
	"net/http"
	"strings"

	"hdhrmux/internal/lineup"
)

// The lineup as an extended M3U playlist, for players and IPTV apps that
// cannot use lineup.json or the HDHomeRun protocol (VLC, Kodi, Jellyfin, Emby,
// TiviMate, Plex through an IPTV tuner, ...). The file is UTF-8, so it is served
// under .m3u and .m3u8 alike (it is a playlist of channels, not HLS: the
// streams themselves are MPEG transport streams over HTTP).
//
// Channels that are not HD carry group-id="SD": without it Channels DVR labels
// every channel of an M3U source as HD.
//
// Channels carry tvg-id equal to the guide number, which is also the channel id
// in the XMLTV guide on /epg, and the header points players at that guide.
//
// Query options:
//
//	hd=1          only HD channels
//	favorites=1   only favourites
//	atsc3=0       leave out ATSC 3.0 channels
//	group=        group-title: source (default, "Antenna"), rf ("545 MHz"), quality (HD / SD), none

func (d *Device) playlistRoutes(mux *http.ServeMux) {
	for _, p := range []string{"/lineup.m3u", "/lineup.m3u8", "/playlist.m3u", "/playlist.m3u8"} {
		mux.HandleFunc("GET "+p, d.handlePlaylist)
	}
}

// m3uAttr makes s safe inside a double-quoted attribute.
func m3uAttr(s string) string {
	return strings.NewReplacer(`"`, "'", "\r", " ", "\n", " ").Replace(s)
}

func (d *Device) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	onlyHD, onlyFav, noATSC3 := q.Get("hd") == "1", q.Get("favorites") == "1", q.Get("atsc3") == "0"
	group := q.Get("group")
	sbase, base := d.streamBase(r), d.requestBase(r)

	d.scanMu.Lock()
	source := d.source
	d.scanMu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U url-tvg=%q x-tvg-url=%q\n", base+"/epg", base+"/epg")
	for _, e := range d.lineup.Entries(false) {
		if e.DRM || (onlyHD && !e.HD) || (onlyFav && !e.Favorite) || (noATSC3 && e.Modulation == "atsc3") {
			continue
		}
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-id=\"%s\" tvg-chno=\"%s\" tvg-name=\"%s\"", m3uAttr(e.GuideNumber), m3uAttr(e.GuideNumber), m3uAttr(e.GuideName))
		if !e.HD {
			// Channels DVR treats every channel of an M3U source as HD unless told otherwise
			b.WriteString(` group-id="SD"`)
		}
		if g := playlistGroup(group, e, source); g != "" {
			fmt.Fprintf(&b, " group-title=\"%s\"", m3uAttr(g))
		}
		name := strings.NewReplacer("\r", " ", "\n", " ").Replace(e.GuideName)
		fmt.Fprintf(&b, ",%s\n%s/auto/v%s\n", name, sbase, e.GuideNumber)
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(b.String()))
}

func playlistGroup(mode string, e lineup.Entry, source string) string {
	switch mode {
	case "none":
		return ""
	case "rf":
		return fmt.Sprintf("%.0f MHz", float64(e.Frequency)/1e6)
	case "quality":
		if e.HD {
			return "HD"
		}
		return "SD"
	}
	if e.Source != "" {
		return e.Source
	}
	return source
}
