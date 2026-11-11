package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const albumID = "11111111-1111-4111-8111-111111111111"
const id1 = "22222222-2222-4222-8222-222222222222"
const id2 = "33333333-3333-4333-8333-333333333333"

func testAsset(id string) Asset {
	sum := sha1.Sum([]byte("picture"))
	return Asset{ID: id, Type: "IMAGE", OriginalFileName: "same.jpg", Checksum: base64.StdEncoding.EncodeToString(sum[:])}
}

type fixture struct {
	items      []Asset
	fail       bool
	wrong      bool
	incomplete bool
	repeat     bool
	downloads  int
	server     *httptest.Server
}

func newFixture(t *testing.T) (*fixture, Config) {
	t.Helper()
	f := &fixture{items: []Asset{testAsset(id1), testAsset(id2)}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			t.Error("missing key")
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/api/albums/"+albumID:
			count := len(f.items)
			if f.incomplete {
				count++
			}
			json.NewEncoder(w).Encode(map[string]any{"id": albumID, "assetCount": count, "updatedAt": "now"})
		case r.URL.Path == "/api/search/metadata":
			if r.Method != "POST" {
				t.Error("search must be POST")
			}
			var body struct {
				Cursor string `json:"cursor"`
				Filter struct {
					AlbumIDs struct {
						Any []string `json:"any"`
					} `json:"albumIds"`
				} `json:"filter"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Filter.AlbumIDs.Any) != 1 || body.Filter.AlbumIDs.Any[0] != albumID {
				t.Error("album filter missing")
			}
			items := f.items
			var next any
			if len(items) > 1 {
				if body.Cursor == "" || f.repeat {
					items = items[:1]
					next = "second"
				} else {
					items = items[1:]
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": items, "nextCursor": next}})
		case strings.HasSuffix(r.URL.Path, "/original") && r.Method == "GET":
			f.downloads++
			if f.fail {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			if f.wrong {
				fmt.Fprint(w, "wrong")
			} else {
				fmt.Fprint(w, "picture")
			}
		default:
			t.Errorf("unexpected mutation/request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(f.server.Close)
	return f, Config{URL: f.server.URL, APIKey: "secret", AlbumID: albumID, Target: t.TempDir()}
}
func exists(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}
func TestDownloadAndNeverDelete(t *testing.T) {
	f, c := newFixture(t)
	foreign := filepath.Join(c.Target, "private.jpg")
	if err := os.WriteFile(foreign, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := synchronize(c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.downloads != 2 {
		t.Fatal(f.downloads)
	}
	if err := synchronize(c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.downloads != 2 {
		t.Fatal("unchanged files downloaded again")
	}
	f.items = f.items[:1]
	if err := synchronize(c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Preserve a removed locally modified image, too.
	if err := os.WriteFile(filepath.Join(c.Target, id2+".jpg"), []byte("local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	f.items = []Asset{}
	if err := synchronize(c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{id1 + ".jpg", id2 + ".jpg", "private.jpg"} {
		exists(t, filepath.Join(c.Target, name))
	}
	var state State
	data, err := os.ReadFile(filepath.Join(c.Target, stateName))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Files) != 2 {
		t.Fatal("history lost")
	}
}
func TestDryRun(t *testing.T) {
	f, c := newFixture(t)
	if err := synchronize(c, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.downloads != 0 {
		t.Fatal("dry-run downloaded")
	}
	if _, err := os.Stat(filepath.Join(c.Target, stateName)); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote state")
	}
}
func TestDownloadFailureAndChecksum(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			f, c := newFixture(t)
			f.fail = !wrong
			f.wrong = wrong
			if err := synchronize(c, false, io.Discard); err == nil {
				t.Fatal("expected failure")
			}
			matches, _ := filepath.Glob(filepath.Join(c.Target, "*.jpg"))
			if len(matches) != 0 {
				t.Fatal("invalid file committed")
			}
			f.fail = false
			f.wrong = false
			if err := synchronize(c, false, io.Discard); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}
func TestIncompleteAndRepeatedPages(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			f, c := newFixture(t)
			f.repeat = repeat
			f.incomplete = !repeat
			if err := synchronize(c, false, io.Discard); err == nil {
				t.Fatal("expected failure")
			}
			if f.downloads != 0 {
				t.Fatal("download from incomplete snapshot")
			}
		})
	}
}
func TestRefuseForeignCollisionAndSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			_, c := newFixture(t)
			path := filepath.Join(c.Target, id1+".jpg")
			var err error
			if symlink {
				err = os.Symlink("/etc/hosts", path)
			} else {
				err = os.WriteFile(path, []byte("foreign"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = synchronize(c, false, io.Discard); err == nil {
				t.Fatal("expected collision failure")
			}
		})
	}
}
func TestPythonStateCompatibility(t *testing.T) {
	f, c := newFixture(t)
	f.items = f.items[:1]
	old := fmt.Sprintf(`{"url":%q,"album_id":%q,"target":%q,"files":{%q:{"name":%q,"checksum":%q}}}`, c.URL+"/api", albumID, c.Target, id2, id2+".jpg", testAsset(id2).Checksum)
	if err := os.WriteFile(filepath.Join(c.Target, stateName), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Target, id2+".jpg"), []byte("picture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := synchronize(c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(c.Target, id2+".jpg"))
	info, err := os.Stat(filepath.Join(c.Target, stateName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("wrong permissions")
	}
}
func TestVideosSkipped(t *testing.T) {
	f, c := newFixture(t)
	f.items[1].Type = "VIDEO"
	if err := synchronize(c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.downloads != 1 {
		t.Fatal("video downloaded")
	}
}
func TestRedirectDoesNotLeakKey(t *testing.T) {
	hit := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer origin.Close()
	c, err := normalize(Config{URL: origin.URL, APIKey: "secret", AlbumID: albumID, Target: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newAPI(c).album(); err == nil {
		t.Fatal("redirect accepted")
	}
	if hit {
		t.Fatal("followed redirect")
	}
}
func TestInstallMigratesUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := installFiles(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config/systemd/user/immich-wallpaper-sync.service"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "python") || !strings.Contains(string(data), "ExecStart=%h/.local/bin/immich-wallpaper-sync") {
		t.Fatal("old service not migrated")
	}
	info, err := os.Stat(filepath.Join(home, ".local/bin/immich-wallpaper-sync"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatal("binary not executable")
	}
}
