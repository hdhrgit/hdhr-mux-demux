package hub

import "testing"

// Real streaminfo replies captured from an HDHR5-4K / HDFX-4K.
func TestParseStreamInfoKeepsQuietSubchannels(t *testing.T) {
	info := "1: 28.1 KCNZ-CD\n4: 20.6 RCTV (no data)\n9: 28.5 JTV (no data)\n13: 28.8 CRTV (no data)\n14: 20.7 CRTV\ntsid=0x0FE5\n"
	es := ParseStreamInfo(info, 515000000, "8vsb", "Antenna")
	got := map[string]string{}
	for _, e := range es {
		got[e.GuideNumber] = e.GuideName
	}
	for gn, name := range map[string]string{"28.1": "KCNZ-CD", "20.6": "RCTV", "28.5": "JTV", "28.8": "CRTV", "20.7": "CRTV"} {
		if got[gn] != name {
			t.Errorf("%s = %q, want %q (all: %v)", gn, got[gn], name, got)
		}
	}
}

func TestParseStreamInfoWithoutTSID(t *testing.T) {
	info := "1: 24.1 DiyaTV\n2: 24.10 JTV\n3: 24.3 Antenna\n8: 30.2 Novlsma\n"
	if es := ParseStreamInfo(info, 533000000, "8vsb", "Antenna"); len(es) != 4 {
		t.Fatalf("got %d programs, want 4", len(es))
	}
}

func TestParseStreamInfoSkipsEncryptedAndControl(t *testing.T) {
	info := "1: 102.1 KTVU-HD (encrypted)\n2: 5.9 Guide (control)\n3: 104.1 KRON-TV\ntsid=0x0178\n"
	es := ParseStreamInfo(info, 177000000, "atsc3", "Antenna")
	if len(es) != 2 || !es[0].DRM || es[0].GuideName != "KTVU-HD" || es[1].GuideNumber != "104.1" {
		t.Fatalf("%+v", es)
	}
}
