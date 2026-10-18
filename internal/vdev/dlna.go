package vdev

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"hdhrmux/internal/hdhrproto"
	"hdhrmux/internal/lineup"
)

// A DLNA/UPnP MediaServer, as real HDHomeRun devices run on /dms. TVs such as
// LG webOS list such servers as video sources and play the channels they
// offer, so the multiplexer presents the same two containers (Favorites and
// Channels) with the same item layout, pointing at its own streams.

const (
	dmsDeviceType = "urn:schemas-upnp-org:device:MediaServer:1"
	// atscDeviceType marks an ATSC 3.0 primary device, which real HDHomeRun 4K
	// devices announce next to their media server.
	atscDeviceType = "urn:schemas-atsc.org:device:primaryDevice:1.0"
	cdType         = "urn:schemas-upnp-org:service:ContentDirectory:1"
	cmType         = "urn:schemas-upnp-org:service:ConnectionManager:1"

	// What the streams are: an MPEG-2 transport stream over HTTP.
	dlnaFlags    = "DLNA.ORG_FLAGS=8D100000000000000000000000000000"
	itemProtocol = "http-get:*:video/mpeg:DLNA.ORG_PN=MPEG_TS_SD_NA_ISO;" + dlnaFlags
)

func (d *Device) dmsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /dms/device.xml", d.handleDMSDescription)
	mux.HandleFunc("GET /device.xml", d.handleDMSDescription)
	mux.HandleFunc("GET /atsc3/device.xml", d.handleATSCDescription)
	mux.HandleFunc("GET /atsc3/{$}", d.handleATSCDescription)
	mux.HandleFunc("GET /dms/ContentDirectory.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		io.WriteString(w, contentDirectorySCPD)
	})
	mux.HandleFunc("GET /dms/ConnectionManager.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		io.WriteString(w, connectionManagerSCPD)
	})
	mux.HandleFunc("POST /dms/ContentDirectory", d.handleContentDirectory)
	mux.HandleFunc("POST /dms/ConnectionManager", d.handleConnectionManager)
	for _, p := range []string{"/dms/ContentDirectory", "/dms/ConnectionManager"} {
		mux.HandleFunc("SUBSCRIBE "+p, d.handleSubscribe)
		mux.HandleFunc("UNSUBSCRIBE "+p, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	}
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (d *Device) udn() string { return "uuid:" + d.uuid() }

// atscUDN identifies the ATSC 3.0 primary device, a device of its own next to the media server.
func (d *Device) atscUDN() string {
	id := strings.ToLower(hdhrproto.FormatDeviceID(d.cfg.DeviceID))
	return fmt.Sprintf("uuid:%s-0000-0000-0001-%s0000", id, id)
}

// handleATSCDescription describes the ATSC 3.0 primary device: identity only,
// as on real devices it has no services.
func (d *Device) handleATSCDescription(w http.ResponseWriter, r *http.Request) {
	id := hdhrproto.FormatDeviceID(d.cfg.DeviceID)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>%s</deviceType>
    <friendlyName>%s ATSC %s</friendlyName>
    <presentationURL>/</presentationURL>
    <manufacturer>%s</manufacturer>
    <modelDescription>%s %s</modelDescription>
    <modelName>%s</modelName>
    <modelNumber>%s</modelNumber>
    <serialNumber>%s</serialNumber>
    <UDN>%s</UDN>
  </device>
</root>
`, atscDeviceType, esc(d.cfg.FriendlyName), id, esc(d.cfg.Manufacturer),
		esc(d.cfg.ModelNumber), esc(d.cfg.FriendlyName), esc(d.cfg.FriendlyName), esc(d.cfg.ModelNumber), id, d.atscUDN())
}

func (d *Device) handleDMSDescription(w http.ResponseWriter, r *http.Request) {
	base := d.requestBase(r)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <dlna:X_DLNADOC>DMS-1.50</dlna:X_DLNADOC>
    <deviceType>%s</deviceType>
    <friendlyName>%s DMS %s</friendlyName>
    <presentationURL>/</presentationURL>
    <manufacturer>%s</manufacturer>
    <modelDescription>%s %s</modelDescription>
    <modelName>%s</modelName>
    <modelNumber>%s</modelNumber>
    <serialNumber>%s</serialNumber>
    <UDN>%s</UDN>
    <serviceList>
      <service>
        <serviceType>%s</serviceType>
        <serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId>
        <SCPDURL>/dms/ConnectionManager.xml</SCPDURL>
        <controlURL>%s/dms/ConnectionManager</controlURL>
        <eventSubURL>%s/dms/ConnectionManager</eventSubURL>
      </service>
      <service>
        <serviceType>%s</serviceType>
        <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
        <SCPDURL>/dms/ContentDirectory.xml</SCPDURL>
        <controlURL>%s/dms/ContentDirectory</controlURL>
        <eventSubURL>%s/dms/ContentDirectory</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>
`, dmsDeviceType, esc(d.cfg.FriendlyName), hdhrproto.FormatDeviceID(d.cfg.DeviceID), esc(d.cfg.Manufacturer),
		esc(d.cfg.ModelNumber), esc(d.cfg.FriendlyName), esc(d.cfg.FriendlyName), esc(d.cfg.ModelNumber),
		hdhrproto.FormatDeviceID(d.cfg.DeviceID), d.udn(), cmType, base, base, cdType, base, base)
}

// handleSubscribe accepts UPnP event subscriptions. The lineup changes rarely
// and clients re-browse, so no events are ever sent.
func (d *Device) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", d.dmsServer())
	w.Header().Set("SID", "uuid:"+d.uuid()+"-"+strconv.FormatInt(int64(len(r.Header.Get("Callback"))), 16))
	w.Header().Set("Timeout", "Second-1800")
	w.WriteHeader(http.StatusOK)
}

func (d *Device) dmsServer() string { return "hdhr-mux/1.0 UPnP/1.0" }

// ---- SOAP ----------------------------------------------------------------

const soapHead = `<?xml version="1.0" encoding="utf-8" standalone="yes"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" soap:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><soap:Body>`
const soapTail = `</soap:Body></soap:Envelope>`

// soapRequest returns the action name and the text of its arguments.
func soapRequest(r *http.Request) (action string, args map[string]string, err error) {
	if a := r.Header.Get("SOAPAction"); a != "" {
		a = strings.Trim(a, `"`)
		if i := strings.LastIndex(a, "#"); i >= 0 {
			action = a[i+1:]
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "", nil, err
	}
	args = map[string]string{}
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	depth, actionDepth := 0, 0
	var cur string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			name := t.Name.Local
			if action == "" && depth == 3 { // Envelope > Body > <Action>
				action = name
			}
			if depth == 3 {
				actionDepth = depth
			} else if actionDepth == 3 && depth == 4 {
				cur = name
			}
		case xml.EndElement:
			if depth == 4 {
				cur = ""
			}
			depth--
		case xml.CharData:
			if cur != "" {
				args[cur] += string(t)
			}
		}
	}
	return action, args, nil
}

func soapFault(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, `%s<soap:Fault><faultcode>soap:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></soap:Fault>%s`,
		soapHead, code, esc(desc), soapTail)
}

func soapReply(w http.ResponseWriter, service, action, body string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Ext", "")
	fmt.Fprintf(w, `%s<u:%sResponse xmlns:u="%s">%s</u:%sResponse>%s`, soapHead, action, service, body, action, soapTail)
}

func (d *Device) handleConnectionManager(w http.ResponseWriter, r *http.Request) {
	action, _, err := soapRequest(r)
	if err != nil {
		soapFault(w, 501, "Action Failed")
		return
	}
	switch action {
	case "GetProtocolInfo":
		src := "http-get:*:video/mpeg:DLNA.ORG_PN=UNKNOWN;" + dlnaFlags + "," + itemProtocol +
			",http-get:*:video/mpeg:DLNA.ORG_PN=AVC_TS_NA_ISO;" + dlnaFlags
		soapReply(w, cmType, action, "<Source>"+esc(src)+"</Source><Sink></Sink>")
	case "GetCurrentConnectionIDs":
		soapReply(w, cmType, action, "<ConnectionIDs>0</ConnectionIDs>")
	case "GetCurrentConnectionInfo":
		soapReply(w, cmType, action, "<RcsID>-1</RcsID><AVTransportID>-1</AVTransportID><ProtocolInfo></ProtocolInfo><PeerConnectionManager></PeerConnectionManager><PeerConnectionID>-1</PeerConnectionID><Direction>Output</Direction><Status>OK</Status>")
	default:
		soapFault(w, 401, "Invalid Action")
	}
}

func (d *Device) handleContentDirectory(w http.ResponseWriter, r *http.Request) {
	action, args, err := soapRequest(r)
	if err != nil {
		soapFault(w, 501, "Action Failed")
		return
	}
	update := strconv.FormatUint(d.lineup.Rev()+1, 10)
	switch action {
	case "GetSystemUpdateID":
		soapReply(w, cdType, action, "<Id>"+update+"</Id>")
	case "GetSearchCapabilities":
		soapReply(w, cdType, action, "<SearchCaps></SearchCaps>")
	case "GetSortCapabilities":
		soapReply(w, cdType, action, "<SortCaps></SortCaps>")
	case "Browse":
		d.browse(w, r, args, update)
	default:
		soapFault(w, 401, "Invalid Action")
	}
}

// ---- Browse ---------------------------------------------------------------

const didlOpen = `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/">`

func container(id, parent, title string) string {
	return fmt.Sprintf(`<container id="%s" parentID="%s" restricted="1"><dc:title>%s</dc:title><upnp:class>object.container</upnp:class><dlna:containerType>Tuner_1_0</dlna:containerType></container>`,
		esc(id), esc(parent), esc(title))
}

func channelItem(e lineup.Entry, parent, streamBase string) string {
	return fmt.Sprintf(`<item id="Channels/v%s" parentID="%s" restricted="1"><dc:title>%s %s</dc:title><upnp:class>object.item.videoItem.videoBroadcast</upnp:class><upnp:channelNr>%s</upnp:channelNr><upnp:channelName>%s</upnp:channelName><res protocolInfo="%s">%s/auto/v%s</res></item>`,
		esc(e.GuideNumber), esc(parent), esc(e.GuideNumber), esc(e.GuideName), esc(e.GuideNumber), esc(e.GuideName),
		esc(itemProtocol), esc(streamBase), esc(e.GuideNumber))
}

// playable lists the channels a TV can play: not encrypted, and (for the
// Favorites container) marked as favourites.
func (d *Device) playable(favorites bool) []lineup.Entry {
	var out []lineup.Entry
	for _, e := range d.lineup.Entries(false) {
		if e.DRM || (favorites && !e.Favorite) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (d *Device) browse(w http.ResponseWriter, r *http.Request, args map[string]string, update string) {
	id, flag := args["ObjectID"], args["BrowseFlag"]
	start, _ := strconv.Atoi(strings.TrimSpace(args["StartingIndex"]))
	count, _ := strconv.Atoi(strings.TrimSpace(args["RequestedCount"]))
	sbase := d.streamBase(r)

	var items []string
	total := 0
	switch {
	case flag == "BrowseMetadata":
		switch {
		case id == "0":
			items = []string{container("0", "-1", "Root")}
		case id == "Channels" || id == "Favorites":
			items = []string{container(id, "0", id)}
		case strings.HasPrefix(id, "Channels/v"):
			e, ok := d.lineup.Find(strings.TrimPrefix(id, "Channels/v"))
			if !ok || e.DRM {
				soapFault(w, 701, "No such object")
				return
			}
			items = []string{channelItem(e, "Channels", sbase)}
		default:
			soapFault(w, 701, "No such object")
			return
		}
		total = len(items)
	case flag == "BrowseDirectChildren":
		var all []string
		switch id {
		case "0":
			all = []string{container("Favorites", "0", "Favorites"), container("Channels", "0", "Channels")}
		case "Channels", "Favorites":
			for _, e := range d.playable(id == "Favorites") {
				all = append(all, channelItem(e, id, sbase))
			}
		default:
			if strings.HasPrefix(id, "Channels/v") {
				all = nil // an item has no children
			} else {
				soapFault(w, 701, "No such object")
				return
			}
		}
		total = len(all)
		if start > len(all) {
			start = len(all)
		}
		end := len(all)
		if count > 0 && start+count < end {
			end = start + count
		}
		items = all[start:end]
	default:
		soapFault(w, 402, "Invalid Args")
		return
	}
	didl := didlOpen + strings.Join(items, "") + `</DIDL-Lite>`
	soapReply(w, cdType, "Browse", fmt.Sprintf("<Result>%s</Result><NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>%s</UpdateID>",
		esc(didl), len(items), total, update))
}
