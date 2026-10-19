package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Storage describes where the state directory (lineup, guide, tuner numbering
// and settings saved from the web interface) lives and whether it will survive
// the container or machine being rebuilt.
type Storage struct {
	Path string `json:"path"`
	// Kind: "disk" (an ordinary directory), "mounted" (a directory or volume
	// given to the container), "anonymous-volume" (a Docker volume nobody
	// named: lost when the container is removed), "container-layer" (inside the
	// container: lost when it is removed or recreated), "memory" (tmpfs).
	Kind      string `json:"kind"`
	Container bool   `json:"container"`
	Writable  bool   `json:"writable"`
	// Warning is empty when the state will be kept.
	Warning string `json:"warning,omitempty"`
}

var reAnonVolume = regexp.MustCompile(`/volumes/[0-9a-f]{64}/_data`)

// mountFor finds the mount holding path in the text of /proc/self/mountinfo.
func mountFor(mountinfo, path string) (mountpoint, root, fstype string) {
	best := -1
	for _, line := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(line)
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if len(f) < 7 || sep < 6 || sep+2 >= len(f) {
			continue
		}
		mp := strings.ReplaceAll(f[4], `\040`, " ")
		if path == mp || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/") || mp == "/" {
			if len(mp) > best {
				best, mountpoint, root, fstype = len(mp), mp, f[3], f[sep+1]
			}
		}
	}
	return
}

// classifyStorage decides what kind of storage path is on.
func classifyStorage(mountinfo, path string, container bool) (kind, warning string) {
	mp, root, fstype := mountFor(mountinfo, path)
	switch {
	case fstype == "tmpfs" || fstype == "ramfs":
		return "memory", "The state directory is in memory and is empty after every restart."
	case !container:
		return "disk", ""
	case mp == "/" || mp == "":
		return "container-layer", "The state directory is inside the container, so the lineup, guide, tuner numbering and settings are lost when the container is removed or recreated. Map a directory or a named volume to it (for example -v hdhr-mux-state:" + path + ")."
	case reAnonVolume.MatchString(root):
		return "anonymous-volume", "The state directory is on an anonymous Docker volume: it survives restarts but is lost when the container is removed. Map a directory or a named volume to it (for example -v hdhr-mux-state:" + path + ")."
	}
	return "mounted", ""
}

// inContainer reports whether we run in Docker or Podman. (LXC and virtual
// machines keep their disks, so they count as ordinary systems.)
func inContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// StorageOf describes the state directory dir.
func StorageOf(dir string) Storage {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	st := Storage{Path: abs, Container: inContainer(), Kind: "disk"}
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		st.Kind, st.Warning = classifyStorage(string(b), abs, st.Container)
	}
	if err := os.MkdirAll(abs, 0o755); err == nil {
		f, err := os.CreateTemp(abs, ".write-test-*")
		if err == nil {
			f.Close()
			os.Remove(f.Name())
			st.Writable = true
		}
	}
	if !st.Writable {
		st.Warning = strings.TrimSpace("The state directory is not writable, so nothing can be saved. " + st.Warning)
	}
	return st
}
