// Immich album downloads for Linux. Never deletes local images.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const stateName = ".immich-wallpaper-sync.json"

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var extensionRE = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

type Config struct {
	URL     string `json:"url"`
	APIKey  string `json:"api_key"`
	AlbumID string `json:"album_id"`
	Target  string `json:"target"`
}
type Entry struct {
	Name     string `json:"name"`
	Checksum string `json:"checksum"`
}
type State struct {
	URL     string           `json:"url"`
	AlbumID string           `json:"album_id"`
	Target  string           `json:"target"`
	Files   map[string]Entry `json:"files"`
}
type Asset struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	OriginalFileName string `json:"originalFileName"`
	Checksum         string `json:"checksum"`
	IsTrashed        bool   `json:"isTrashed"`
}
type Album struct {
	ID         string `json:"id"`
	AssetCount *int   `json:"assetCount"`
	UpdatedAt  string `json:"updatedAt"`
	Modified   string `json:"lastModifiedAssetTimestamp"`
}
type Search struct {
	Assets struct {
		Items      []Asset         `json:"items"`
		NextCursor json.RawMessage `json:"nextCursor"`
	} `json:"assets"`
}
type API struct {
	config Config
	client *http.Client
}

func expandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}
func normalize(c Config) (Config, error) {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	u, err := url.Parse(c.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("ungültige Immich-URL")
	}
	if !strings.HasSuffix(c.URL, "/api") {
		c.URL += "/api"
	}
	if !uuidRE.MatchString(c.AlbumID) {
		return c, errors.New("ungültige Album-ID")
	}
	c.AlbumID = strings.ToLower(c.AlbumID)
	if strings.TrimSpace(c.APIKey) == "" {
		return c, errors.New("API-Key fehlt")
	}
	if c.Target == "" {
		return c, errors.New("Zielordner fehlt")
	}
	c.Target, err = expandPath(c.Target)
	return c, err
}
func newAPI(c Config) *API {
	return &API{c, &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("HTTP-Weiterleitung abgelehnt; direkte Immich-URL verwenden")
	}}}
}
func (a *API) request(path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(data)
	}
	method := "GET"
	if body != nil {
		method = "POST"
	}
	req, err := http.NewRequest(method, a.config.URL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", a.config.APIKey)
	req.Header.Set("Accept", "application/json, application/octet-stream")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: URL, API-Key und Rechte album.read, asset.read, asset.download prüfen", resp.StatusCode)
	}
	return resp, nil
}
func (a *API) getJSON(path string, body, out any) error {
	r, err := a.request(path, body)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(out)
}
func (a *API) album() (Album, error) {
	var album Album
	err := a.getJSON("/albums/"+a.config.AlbumID, nil, &album)
	if err != nil {
		return album, err
	}
	if album.ID != a.config.AlbumID || album.AssetCount == nil || *album.AssetCount < 0 || album.UpdatedAt == "" {
		return album, errors.New("ungültige Albumantwort")
	}
	return album, nil
}
func sameAlbum(a, b Album) bool {
	return a.ID == b.ID && *a.AssetCount == *b.AssetCount && a.UpdatedAt == b.UpdatedAt && a.Modified == b.Modified
}
func (a *API) snapshot() (map[string]Asset, error) {
	before, err := a.album()
	if err != nil {
		return nil, err
	}
	assets := map[string]Asset{}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= 10000 {
			return nil, errors.New("zu viele Ergebnisseiten")
		}
		body := map[string]any{"filter": map[string]any{"albumIds": map[string]any{"any": []string{a.config.AlbumID}}, "trashedAt": map[string]any{"eq": nil}}, "size": 1000, "withExif": false, "withPeople": false}
		if cursor != "" {
			body["cursor"] = cursor
		}
		var result Search
		if err := a.getJSON("/search/metadata", body, &result); err != nil {
			return nil, err
		}
		if len(result.Assets.NextCursor) == 0 || result.Assets.Items == nil {
			return nil, errors.New("unvollständige Suchantwort; Immich >=3.2 erforderlich")
		}
		for _, asset := range result.Assets.Items {
			if !uuidRE.MatchString(asset.ID) {
				return nil, errors.New("ungültige Asset-ID")
			}
			asset.ID = strings.ToLower(asset.ID)
			if _, ok := assets[asset.ID]; ok {
				return nil, errors.New("doppelte Suchergebnisse")
			}
			assets[asset.ID] = asset
		}
		cursor = ""
		if err := json.Unmarshal(result.Assets.NextCursor, &cursor); err != nil {
			return nil, err
		}
		if cursor == "" {
			break
		}
		if seen[cursor] || len(result.Assets.Items) == 0 {
			return nil, errors.New("ungültige Seitennavigation")
		}
		seen[cursor] = true
	}
	after, err := a.album()
	if err != nil {
		return nil, err
	}
	if !sameAlbum(before, after) || len(assets) != *after.AssetCount {
		return nil, errors.New("Album verändert oder Liste unvollständig; erneut versuchen")
	}
	for id, asset := range assets {
		if asset.Type != "IMAGE" || asset.IsTrashed {
			delete(assets, id)
		}
	}
	return assets, nil
}
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sync-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
func atomicJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0600)
}
func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
func filename(asset Asset) (string, error) {
	ext := strings.ToLower(filepath.Ext(asset.OriginalFileName))
	if !uuidRE.MatchString(asset.ID) || !extensionRE.MatchString(ext) {
		return "", errors.New("ungültiger Bilddateiname oder Asset-ID")
	}
	sum, err := base64.StdEncoding.DecodeString(asset.Checksum)
	if err != nil || len(sum) != sha1.Size {
		return "", errors.New("fehlende oder ungültige Bildprüfsumme")
	}
	return asset.ID + ext, nil
}
func download(a *API, asset Asset, path string, state *State, statePath string) error {
	resp, err := a.request("/assets/"+asset.ID+"/original", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return errors.New("HTML statt Bild erhalten; URL prüfen")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = io.Copy(f, resp.Body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	sum, err := fileChecksum(name)
	if err != nil {
		return err
	}
	if sum != asset.Checksum {
		return errors.New("Prüfsumme des Downloads stimmt nicht")
	}
	state.Files[asset.ID] = Entry{filepath.Base(path), asset.Checksum}
	// Persist ownership before rename, so a crash leaves a recoverable download.
	if err = atomicJSON(statePath, state); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func synchronize(c Config, dry bool, out io.Writer) error {
	var err error
	c, err = normalize(c)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(c.Target, 0700); err != nil {
		return err
	}
	c.Target, err = filepath.EvalSymlinks(c.Target)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(c.Target, ".immich-wallpaper-sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			fmt.Fprintln(out, "Synchronisierung läuft bereits.")
			return nil
		}
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	state := State{c.URL, c.AlbumID, c.Target, map[string]Entry{}}
	statePath := filepath.Join(c.Target, stateName)
	data, err := os.ReadFile(statePath)
	if err == nil {
		if err = json.Unmarshal(data, &state); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.URL != c.URL || state.AlbumID != c.AlbumID || state.Target != c.Target || state.Files == nil {
		return errors.New("Zielordner gehört zu anderer Konfiguration oder ungültiger Status")
	}
	for id, e := range state.Files {
		if !uuidRE.MatchString(id) || !strings.HasPrefix(e.Name, id) || !extensionRE.MatchString(strings.TrimPrefix(e.Name, id)) {
			return errors.New("ungültiger Dateiname im lokalen Status")
		}
	}
	a := newAPI(c)
	assets, err := a.snapshot()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		asset := assets[id]
		name, err := filename(asset)
		if err != nil {
			return err
		}
		path := filepath.Join(c.Target, name)
		entry, tracked := state.Files[id]
		if tracked && entry.Name != name {
			return errors.New("Dateiendung geändert; neuen Zielordner verwenden")
		}
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("keine reguläre Datei im Downloadziel: %s", name)
			}
			if !tracked || entry.Name != name {
				return fmt.Errorf("nicht verwaltete Datei im Downloadziel: %s", name)
			}
			sum, err := fileChecksum(path)
			if err != nil {
				return err
			}
			if sum == asset.Checksum {
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if dry {
			fmt.Fprintln(out, "Würde herunterladen:", asset.OriginalFileName)
			continue
		}
		fmt.Fprintln(out, "Lade herunter:", asset.OriginalFileName)
		if err = download(a, asset, path, &state, statePath); err != nil {
			return err
		}
	}
	// Deliberately no pruning: keep old files AND their ownership records.
	if !dry {
		if err = atomicJSON(statePath, state); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "%d Bilder im Album. ", len(assets))
	if dry {
		fmt.Fprintln(out, "Probelauf abgeschlossen.")
	} else {
		fmt.Fprintln(out, "Synchronisierung abgeschlossen.")
	}
	return nil
}
func defaultConfig() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config/immich-wallpaper-sync/config.json")
}
func readConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return normalize(c)
}
func prompt(r *bufio.Reader, label string) (string, error) {
	fmt.Print(label)
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}
func secret(r *bufio.Reader) (string, error) {
	fd := os.Stdin.Fd()
	var old syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	if errno != 0 {
		return "", errors.New("Setup benötigt ein Terminal für die verdeckte API-Key-Eingabe")
	}
	next := old
	next.Lflag &^= syscall.ECHO
	_, _, errno = syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&next)), 0, 0, 0)
	if errno != 0 {
		return "", errno
	}
	defer func() {
		syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old)), 0, 0, 0)
		fmt.Println()
	}()
	return prompt(r, "Immich-API-Key (Eingabe unsichtbar): ")
}
func installFiles() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	bin := filepath.Join(home, ".local/bin/immich-wallpaper-sync")
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		return err
	}
	if err = atomicWrite(bin, data, 0755); err != nil {
		return err
	}
	units := filepath.Join(home, ".config/systemd/user")
	if err = os.MkdirAll(units, 0755); err != nil {
		return err
	}
	service := `[Unit]
Description=Immich-Album Hintergründe lokal nachladen

[Service]
Type=oneshot
ExecStart=%h/.local/bin/immich-wallpaper-sync
UMask=0077
TimeoutStartSec=30min
`
	timer := `[Unit]
Description=Immich-Hintergründe stündlich aktualisieren

[Timer]
OnStartupSec=2min
OnUnitInactiveSec=1h

[Install]
WantedBy=timers.target
`
	if err = atomicWrite(filepath.Join(units, "immich-wallpaper-sync.service"), []byte(service), 0644); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(units, "immich-wallpaper-sync.timer"), []byte(timer), 0644)
}
func setup(path string) error {
	if path != defaultConfig() {
		return errors.New("Setup/Installation verwenden nur den Standard-Konfigurationspfad")
	}
	if _, err := os.Stat(path); err == nil {
		return errors.New("Konfiguration existiert; --install zum Übernehmen verwenden")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	r := bufio.NewReader(os.Stdin)
	var c Config
	var err error
	c.URL, err = prompt(r, "Immich-URL: ")
	if err != nil {
		return err
	}
	value, err := prompt(r, "Album-URL oder Album-ID von „Hintergründe“: ")
	if err != nil {
		return err
	}
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil {
			return err
		}
		value = filepath.Base(strings.TrimRight(u.Path, "/"))
	}
	c.AlbumID = value
	c.APIKey, err = secret(r)
	if err != nil {
		return err
	}
	c.Target, err = prompt(r, "Zielordner [~/Bilder/Hintergründe]: ")
	if err != nil {
		return err
	}
	if c.Target == "" {
		c.Target = "~/Bilder/Hintergründe"
	}
	c, err = normalize(c)
	if err != nil {
		return err
	}
	if _, err = newAPI(c).album(); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err = atomicJSON(path, c); err != nil {
		return err
	}
	return installFiles()
}
func run() error {
	config := flag.String("config", defaultConfig(), "Konfigurationsdatei")
	dry := flag.Bool("dry-run", false, "Änderungen nur anzeigen (legt ggf. Zielordner und Lockdatei an)")
	setupFlag := flag.Bool("setup", false, "Interaktive Ersteinrichtung")
	install := flag.Bool("install", false, "Binary und systemd-Units mit bestehender Konfiguration installieren")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unerwartete Argumente")
	}
	if *setupFlag && *install {
		return errors.New("entweder --setup oder --install verwenden")
	}
	if *setupFlag || *install {
		if *config != defaultConfig() {
			return errors.New("Setup/Installation verwenden nur den Standard-Konfigurationspfad")
		}
		var err error
		if *setupFlag {
			err = setup(*config)
		} else {
			if _, err = readConfig(*config); err == nil {
				err = installFiles()
			}
		}
		if err != nil {
			return err
		}
		fmt.Println("Installiert. Weiter mit:\n  systemctl --user daemon-reload\n  ~/.local/bin/immich-wallpaper-sync --dry-run\n  ~/.local/bin/immich-wallpaper-sync\n  systemctl --user enable --now immich-wallpaper-sync.timer")
		return nil
	}
	path, err := expandPath(*config)
	if err != nil {
		return err
	}
	c, err := readConfig(path)
	if err != nil {
		return err
	}
	return synchronize(c, *dry, os.Stdout)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
}
